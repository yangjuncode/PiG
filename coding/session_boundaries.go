// Ports packages/coding-agent/src/core/agent-session.ts.

package coding

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding/extension"
	"github.com/MichaelKinsy/PiG/extensions/sdk/json"
	"github.com/MichaelKinsy/PiG/internal/boundarywire"
	icodingagent "github.com/MichaelKinsy/PiG/internal/codingagent"
	"github.com/MichaelKinsy/PiG/internal/codingagent/compaction"
)

// installAgentBoundaryHooks runs actionable turn_end before the Agent chooses its next turn. An earlier explicit end decision wins; an invalid extension continuation does not suppress natural tool or queue scheduling.
//
// 把 Session 的 turn_end 扩展边界塞进 Agent 的 FinishTurn 钩子：
// 每个自然回合结束时，先让扩展看/改（可写草稿条目、可要求续轮），
// 再跑调用方原 FinishTurn。裁决合并规则——原来已判 end 不再被扩展
// 的 continue 推翻；扩展 continue 或原判 continue 都合成 continue；
// 否则 nil（交给自然规则）。boundaryDispatchedMessage 标记本回合已
// 走边界，让 handleAgentEvent 的 TurnEndEvent 分支不重复派发。
func (s *Session) installAgentBoundaryHooks() {
	s.lastActivityOutcome = extension.AgentActivityCompleted
	previous := s.agent.FinishTurnHook()
	s.agent.SetFinishTurn(func(ctx context.Context, turn agent.AgentTurnContext) (*agent.AgentTurnDecision, error) {
		s.boundaryDispatchedMessage = turn.Message
		continued, err := s.dispatchTurnEndBoundary(ctx, agent.TurnEndEvent{Message: agent.AgentMessage{Assistant: turn.Message}, ToolResults: turn.ToolResults})
		if err != nil {
			return nil, err
		}
		var decision *agent.AgentTurnDecision
		if previous != nil {
			decision, err = previous(ctx, turn)
			if err != nil {
				return nil, err
			}
		}
		if decision != nil && decision.Action == agent.AgentTurnEnd {
			return decision, nil
		}
		if continued || decision != nil && decision.Action == agent.AgentTurnContinue {
			return &agent.AgentTurnDecision{Action: agent.AgentTurnContinue}, nil
		}
		return nil, nil
	})
}

// handleAgentEvent is the Session's awaited Agent subscription. Queue consumption notifies listeners before message_start handlers. Synthetic failed turns have no FinishTurn dispatch, so their boundary runs here before public listeners.
//
// Session 侧唯一挂到 Agent 的事件总线入口，每个 agent 事件都会过
// 这道序列（在 agent goroutine 上同步执行）：
//  1. recordNestedCalls 记账嵌套工具调用；
//  2. 按事件类型做副作用：user 消息核销队列双写、agent_start 归零
//     回合计数、turn_end 对"未走 FinishTurn 边界的回合"补发边界
//     （合成失败回合没有 FinishTurn 的机会）；
//  3. 非 turn_end 事件先走扩展事件钩子 + 观察域让渡（对应上游两个
//     microtask 边界）；
//  4. emitSynchronousSessionEvent 同步投给 Session 监听器；
//  5. message_end 落盘持久化；
//  6. 包 publishedSessionEvent 入队，等持久化完成后才对 events
//     通道发布；
//  7. turn_end 末尾记下本回合工具结果、冲刷挂起的自定义消息。
func (s *Session) handleAgentEvent(ctx context.Context, event agent.AgentEvent) error {
	s.agentEventHold.wait()
	// upstream: agent-session.ts:1104-1106 (_handleAgentEvent): an abort requested while the run was starting, e.g. during
	// before_agent_start, stops it here. agent_start is awaited before the first request.
	if _, ok := event.(agent.AgentStartEvent); ok && s.runState.abortRequested.Load() {
		s.agent.Abort()
	}
	// Record the calls a tool made through ctx.executeTool() and their usage on its result message.
	s.recordNestedCalls(event)
	switch event := event.(type) {
	case agent.MessageStartEvent:
		if event.Message.User != nil {
			s.overflowRecoveryAttempted.Store(false)
			if update, removed := s.consumeQueuedMessage(event.Message); removed {
				s.notifyAgentEventListeners(update)
				s.publishQueueEvent(update)
			}
		}
	case agent.AgentStartEvent:
		s.boundaryTurnIndex = 0
	case agent.TurnEndEvent:
		if event.Message.Assistant != nil && event.Message.Assistant != s.boundaryDispatchedMessage {
			if _, err := s.dispatchTurnEndBoundary(ctx, event); err != nil {
				return err
			}
		}
		s.boundaryDispatchedMessage = nil
		s.boundaryTurnIndex++
	}
	if _, turnEnd := event.(agent.TurnEndEvent); !turnEnd {
		s.extensionEventHook(event)
		// upstream: packages/coding-agent/src/core/agent-session.ts:1250-1327 `_emitExtensionEvent` awaits `runner.emit(...)` and returns, and `_handleAgentEvent` awaits that promise (:1098). Each await is one microtask, so the public listeners run two reactions after the emit call settles; emitSynchronousSessionEvent below is the second.
		if observation := agent.EventObservation(event); observation != nil {
			observation.Yield()
		}
	}
	s.emitSynchronousSessionEvent(event, true)
	var persistErr error
	if end, ok := event.(agent.MessageEndEvent); ok {
		persistErr = s.persistMessage(end.Message)
	}
	// Publish after persistence but before pending custom messages. Later Agent subscribers observe the completed Session transaction.
	s.emitPublishedEvent(event)
	if persistErr != nil {
		return persistErr
	}
	if _, ok := event.(agent.TurnEndEvent); ok {
		return s.flushPendingCustomMessages()
	}
	return nil
}

func (s *Session) dispatchTurnEndBoundary(ctx context.Context, event agent.TurnEndEvent) (bool, error) {
	// 先按 stop reason 给本回合定 activity 结局——before_settle/turn_end
	// 事件的 BoundaryState 就带这个值给扩展看。
	s.lastActivityOutcome = extension.AgentActivityCompleted
	if message := event.Message.Assistant; message != nil {
		switch message.StopReason {
		case ai.StopReasonError:
			s.lastActivityOutcome = extension.AgentActivityError
		case ai.StopReasonAborted:
			s.lastActivityOutcome = extension.AgentActivityAborted
		}
	}
	runner := s.currentRunner()
	if runner == nil || !runner.HasHandlers(icodingagent.EventTurnEnd) {
		return false, nil
	}
	// 解析 assistant/工具结果各自的磁盘条目 ID 交给扩展（扩展按
	// entry id 写草稿/编辑）；解析不到 assistant 条目是 bug 级状况，
	// 记扩展错误并跳过边界。
	event = s.turnEndWithEntryIDs(event)
	for i := range event.ToolResults {
		s.rememberMessageEntry(agent.AgentMessage{ToolResult: &event.ToolResults[i]}, event.ToolResultEntryIDs[i])
	}
	if event.MessageEntryID == "" {
		runner.EmitError(&extension.ExtensionError{ExtensionPath: "<boundary>", Event: icodingagent.EventTurnEnd, Error: "turn_end could not resolve the persisted assistant entry ID"})
		return false, nil
	}
	// upstream: agent-session.ts _dispatchTurnEndBoundary hands the ToolResultMessage objects on, so JSON writes them as the session does, including details: null.
	results := make([]extension.ToolResultMessage, len(event.ToolResults))
	copy(results, event.ToolResults)
	ids := make([]string, 0, len(event.ToolResultEntryIDs))
	for _, id := range event.ToolResultEntryIDs {
		if id != "" {
			ids = append(ids, id)
		}
	}
	base := extension.TurnEndEvent{Type: icodingagent.EventTurnEnd, TurnIndex: s.boundaryTurnIndex, Message: event.Message, ToolResults: results, MessageEntryID: event.MessageEntryID, ToolResultEntryIds: ids, BoundaryState: &extension.BoundaryState{Outcome: s.lastActivityOutcome}}
	// Upstream emitBoundary takes no abort signal, so an aborted run's turns
	// still reach turn_end handlers.
	// EmitBoundary 用 WithoutCancel：被中止的回合也要让 turn_end
	// 扩展看到结局。期间扩展可反复要 buildBoundaryContext 预览草稿
	// 效果，最后返回 Entries（待提交草稿）+ Continue 裁决。
	dispatchCtx, wire := boundarywire.WithSessionCache(context.WithoutCancel(ctx), &s.boundaryEncodings)
	result, err := runner.EmitBoundary(dispatchCtx, base, func(drafts []extension.SessionBoundaryDraft) (extension.BoundaryContextPreview, error) {
		return s.recordedBoundaryContext(wire, drafts, icodingagent.EventTurnEnd)
	})
	if err != nil {
		return false, err
	}
	if err := s.commitBoundaryDrafts(result.Entries); err != nil {
		return false, err
	}
	if !result.Continue {
		return false, nil
	}
	// upstream: agent-session.ts _dispatchTurnEndBoundary builds the final context only when a handler asked to continue. The check reuses the last preview's projection while the session is as that preview found it.
	final, err := s.recordedBoundaryContext(wire, nil, icodingagent.EventTurnEnd)
	if err != nil {
		return false, err
	}
	// 扩展要求续轮但没有可跑的内容：记错误并忽略其裁决。
	if !final.CanContinue {
		s.reportInvalidBoundaryContinuation(icodingagent.EventTurnEnd)
		return false, nil
	}
	return true, nil
}

// runBeforeSettleBoundary 是运行落定前的最后一道扩展边界：
// agent_before_settle 处理器可以追加条目/自定义消息并要求续跑
// （比如"目标没达成就再让模型补一句"）。isBeforeSettle 标记期间
// RequestAbort 不硬取消而是等边界返回；边界结束后校验 CanContinue
// 防"没内容可跑还要求续"。
func (s *Session) runBeforeSettleBoundary(ctx context.Context) (bool, error) {
	runner := s.currentRunner()
	if runner == nil || !runner.HasHandlers(icodingagent.EventAgentBeforeSettle) {
		return s.agent.HasQueuedMessages(), nil
	}
	s.runState.mu.Lock()
	if s.runState.abortRequested.Load() {
		s.runState.mu.Unlock()
		return false, nil
	}
	s.runState.isBeforeSettle = true
	s.runState.mu.Unlock()
	defer func() { s.runState.mu.Lock(); s.runState.isBeforeSettle = false; s.runState.mu.Unlock() }()
	base := &extension.AgentBeforeSettleEvent{Type: icodingagent.EventAgentBeforeSettle, BoundaryState: extension.BoundaryState{Outcome: s.lastActivityOutcome}}
	dispatchCtx, wire := boundarywire.WithSessionCache(ctx, &s.boundaryEncodings)
	result, err := runner.EmitBoundary(dispatchCtx, base, func(drafts []extension.SessionBoundaryDraft) (extension.BoundaryContextPreview, error) {
		return s.recordedBoundaryContext(wire, drafts, icodingagent.EventAgentBeforeSettle)
	})
	if err != nil {
		return false, err
	}
	if err := s.commitBoundaryDrafts(result.Entries); err != nil {
		return false, err
	}
	if err := s.flushPendingCustomMessages(); err != nil {
		return false, err
	}
	// The check after the handlers reuses the last preview's projection while the session is as that preview found it.
	final, err := s.recordedBoundaryContext(wire, nil, icodingagent.EventAgentBeforeSettle)
	if err != nil {
		return false, err
	}
	if s.runState.abortRequested.Load() {
		return false, nil
	}
	shouldContinue := result.Continue || s.agent.HasQueuedMessages()
	if shouldContinue && !final.CanContinue {
		if result.Continue {
			s.reportInvalidBoundaryContinuation(icodingagent.EventAgentBeforeSettle)
		}
		return false, nil
	}
	return shouldContinue, nil
}

func (s *Session) reportInvalidBoundaryContinuation(event string) {
	s.currentRunner().EmitError(&extension.ExtensionError{ExtensionPath: "<boundary>", Event: event, Error: event + " requested continuation without runnable model context"})
}

func (s *Session) commitBoundaryDrafts(drafts []extension.SessionBoundaryDraft) error {
	appended, err := applyBoundaryDrafts(s.inner, drafts)
	if err != nil {
		return err
	}
	s.refreshContext()
	for _, entry := range appended {
		s.emitOrderedEventSync(agent.EntryAppendedEvent{Entry: entry.Raw()})
	}
	return nil
}

// createBoundaryPreviewManager 造一份"当前分支 + 草稿"的影子
// Session：扩展在边界回调里预览"这些草稿如果提交了上下文会长成
// 什么样"，预览完丢弃，不碰真实 JSONL。
func (s *Session) createBoundaryPreviewManager(drafts []extension.SessionBoundaryDraft) (*icodingagent.Session, error) {
	var branch []icodingagent.SessionEntry
	if leaf := s.inner.GetLeafID(); leaf != nil {
		// The preview walks parent links, so it takes the whole branch.
		branch = s.inner.GetBranch(*leaf)
	}
	preview := icodingagent.NewBranchSession(s.inner.GetHeader(), branch)
	if _, err := applyBoundaryDrafts(preview, drafts); err != nil {
		return nil, err
	}
	return preview, nil
}

// boundaryProjection is the projection of a preview manager holding the session's branch and then drafts (agent-session.ts _buildBoundaryContext). With no drafts the preview holds exactly the branch, whose entries keep their ids and parent links, so its projection is the session's own and the copy is skipped. That projection's messages are the session's own objects, as Pi's are: the caller reads them and never modifies them.
//
// 边界上下文用的投影：有草稿（扩展预写条目）时先建预览 manager
// 把草稿叠到分支尾再投影；无草稿直接拿会话自己的投影视图，
// 消息就是会话原对象——只读不改。
func (s *Session) boundaryProjection(drafts []extension.SessionBoundaryDraft) (icodingagent.SessionProjection, error) {
	if len(drafts) == 0 {
		return s.inner.BuildSessionProjectionView(), nil
	}
	preview, err := s.createBoundaryPreviewManager(drafts)
	if err != nil {
		return icodingagent.SessionProjection{}, err
	}
	return preview.BuildSessionProjection(), nil
}

func applyBoundaryDrafts(manager *icodingagent.Session, drafts []extension.SessionBoundaryDraft) ([]icodingagent.SessionEntry, error) {
	appended := make([]icodingagent.SessionEntry, 0, len(drafts))
	for _, draft := range drafts {
		var id string
		var err error
		switch draft.Type {
		case "custom":
			id, err = manager.AppendCustomEntry(draft.CustomType, draft.Data)
		case "custom_message":
			id, err = manager.AppendCustomMessageEntry(draft.CustomType, draft.Content, draft.Display, draft.Details)
		case "context_edit":
			var replacement *icodingagent.ContextEditReplacement
			if raw := bytes.TrimSpace(draft.Replacement); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
				replacement = &icodingagent.ContextEditReplacement{}
				if err = json.Unmarshal(raw, replacement); err != nil {
					return nil, fmt.Errorf("context_edit replacement: %w", err)
				}
			}
			id, err = manager.AppendContextEdit(draft.TargetID, replacement)
		case "compaction":
			firstKept := ""
			if draft.FirstKeptEntryID != nil {
				firstKept = *draft.FirstKeptEntryID
			}
			tokensBefore := compaction.EstimateProjectedContextTokens(manager.BuildSessionProjection(), manager.GetContextBranch()).Tokens
			id, err = manager.AppendCompaction(draft.Summary, firstKept, tokensBefore, draft.Details, true, draft.Usage)
		default:
			return nil, fmt.Errorf("unsupported session boundary draft type %q", draft.Type)
		}
		if err != nil {
			return nil, err
		}
		entry, found := manager.GetEntry(id)
		if !found {
			return nil, fmt.Errorf("boundary entry %s was not appended", id)
		}
		appended = append(appended, entry)
	}
	return appended, nil
}

// recordedBoundaryContext is projectBoundaryContext for a boundary dispatch: it records the preview and the inputs of its projected part in the dispatch's wire cache, so subprocess handlers share one encoding while those inputs stay the same. While they stay the same and no handler that reads Go values has held the latest preview, the next preview reuses its projected part instead of projecting the session again: a projection of the same session state is equal to it.
//
// 带 wire 缓存的边界预览：把投影输入（会话+修订号）记进派发缓存，
// 子进程处理器共享同一份编码；输入未变且无"读 Go 值"的处理器拿走
// 过最新预览时，下次直接复用投影部分，不重投影——同一会话状态的
// 投影必然相等。
func (s *Session) recordedBoundaryContext(wire *boundarywire.Cache, drafts []extension.SessionBoundaryDraft, boundary string) (extension.BoundaryContextPreview, error) {
	inner := s.inner
	revision := inner.Revision()
	key := boundarywire.Key{Session: inner, Revision: revision}
	if len(drafts) == 0 {
		if projected, sources, shared, ok := wire.Projected(key); ok {
			preview := s.completeBoundaryContext(projected, boundary)
			wire.Built(preview, key, true, sources)
			if shared {
				wire.Shared()
			}
			return preview, nil
		}
	}
	preview, sources, shared, err := s.projectBoundaryContext(drafts, boundary)
	if err != nil {
		return preview, err
	}
	cacheable := len(drafts) == 0 && inner == s.inner && revision == inner.Revision()
	wire.Built(preview, key, cacheable, sources)
	if shared {
		wire.Shared()
	}
	return preview, nil
}

// projectBoundaryContext is agent-session.ts _buildBoundaryContext with each context entry's SourceEntry unset and its record returned in sources: Pi's entry is the object the record parses to, which boundarywire.Cache.Deliver decodes for a handler that reads Go values. Without drafts, the preview's messages are the session's own objects, as Pi's are (shared): Deliver hands a handler that reads Go values copies of them.
//
// 上游 _buildBoundaryContext：预览里每条 context entry 的
// SourceEntry 置空、原始记录单独进 sources 返回——上游那条 entry
// 就是记录解析出的对象；boundarywire.Deliver 为"读 Go 值"的处理器
// 解码它。无草稿时消息即会话原对象（shared=true）。
func (s *Session) projectBoundaryContext(drafts []extension.SessionBoundaryDraft, boundary string) (preview extension.BoundaryContextPreview, sources []json.RawMessage, shared bool, err error) {
	projection, err := s.boundaryProjection(drafts)
	if err != nil {
		return extension.BoundaryContextPreview{}, nil, false, err
	}
	shared = len(drafts) == 0
	pass := s.boundaryRecords.begin()
	entries := make([]extension.ProjectedSessionEntry, len(projection.Entries))
	sources = make([]json.RawMessage, len(projection.Entries))
	for i, projected := range projection.Entries {
		messages := make([]extension.AgentMessage, len(projected.Messages))
		copy(messages, projected.Messages)
		entries[i] = extension.ProjectedSessionEntry{Messages: messages}
		sources[i] = s.boundaryRecords.record(projected.SourceEntry, pass)
	}
	s.boundaryRecords.forget(pass)
	contextMessages := make([]extension.AgentMessage, len(projection.Messages))
	copy(contextMessages, projection.Messages)
	llm := agent.ConvertToLLM(projection.Messages)
	projected := extension.BoundaryContextPreview{ContextEntries: entries, ContextMessages: contextMessages, LLMMessages: llm}
	return s.completeBoundaryContext(projected, boundary), sources, shared, nil
}

// completeBoundaryContext adds to the projected part of a preview (contextEntries, contextMessages, llmMessages) the pending messages and canContinue, which read the run state at each build.
//
// 给投影部分补上"每次都现读运行态"的字段：pendingMessages（队列
// steering/followUp + 待注入 custom）与 canContinue（上下文尾部非
// assistant、有待注入 custom、或队列为空之外还有待发消息）。
func (s *Session) completeBoundaryContext(projected extension.BoundaryContextPreview, boundary string) extension.BoundaryContextPreview {
	hasNonSystem, finalAssistant := false, false
	for _, message := range projected.LLMMessages {
		_, system := message.(ai.SystemMessage)
		hasNonSystem = hasNonSystem || !system
		_, finalAssistant = message.(ai.AssistantMessage)
	}
	s.runState.mu.Lock()
	custom := slices.Clone(s.runState.pendingCustom)
	s.runState.mu.Unlock()
	pending := slices.Concat(s.agent.PeekQueuedMessages(), custom)
	pendingMessages := make([]extension.AgentMessage, len(pending))
	// CanContinue = "续一轮有没有东西可跑"：上下文尾部不是 assistant
	// （模型有话可接）、有待注入的自定义消息、或队列里还有消息——
	// 三种之一即可；扩展据此决定要不要 Continue。
	copy(pendingMessages, pending)
	canContinue := hasNonSystem && !finalAssistant || len(custom) > 0 || (boundary == icodingagent.EventTurnEnd || finalAssistant) && s.agent.HasQueuedMessages()
	return extension.BoundaryContextPreview{ContextEntries: projected.ContextEntries, ContextMessages: projected.ContextMessages, LLMMessages: projected.LLMMessages, PendingMessages: pendingMessages, CanContinue: canContinue}
}

// boundaryRecords keeps, from one boundary projection to the next, the records it handed out for message entries the session holds without
// their records (it holds the appended object, as Pi does). Such an entry encodes its record again on each read, and a dispatch hands its
// subprocess handlers every context entry's record, whose encoding boundarywire keeps by the record's bytes: keeping the record a projection
// handed out encodes each entry once while it stays in the context, and lets boundarywire find its kept encoding. Each projection keeps only
// the records it used.
//
// 跨投影的"已发记录"缓存：会话里按追加对象持有的消息条目没有
// 原始 JSON 记录，每次读都要重编码；这里按（条目ID+消息对象指针）
// 记上次发出去的那份 raw，让 boundarywire 按字节找到已缓存编码——
// 同一消息在上下文里待着期间只编码一次。每趟投影只留用过的记录。
type boundaryRecords struct {
	mu   sync.Mutex
	pass uint64
	kept map[boundaryRecordKey]keptBoundaryRecord
}

// boundaryRecordKey names a message entry by its id and the message object it holds, so a record is handed out only for the value it encodes.
//
// 缓存键 = 条目 ID + 四个消息变体指针：同一条目对象换了消息指针
// 就是不同键——记录只对"它编码的那个值"有效。
type boundaryRecordKey struct {
	id         string
	system     *ai.SystemMessage
	user       *agent.UserMessage
	assistant  *agent.AssistantMessage
	toolResult *agent.ToolResultMessage
}

// keptBoundaryRecord = 一份已发记录 + 它最后被哪趟投影用过。
type keptBoundaryRecord struct {
	raw  json.RawMessage
	pass uint64
}

// begin starts a projection's pass.
//
// 开一趟投影的代次（pass 单调递增）：本趟用到的记录都打上这个
// 代次，结束后 forget 清掉没被打上的旧记录。
func (b *boundaryRecords) begin() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pass++
	return b.pass
}

// record returns entry's record, the one an earlier projection handed out when it names the same message object.
//
// 取条目记录：非消息条目直接取 Raw；消息条目按（ID+对象指针）查
// 缓存，命中复用并刷新代次，未中编码一份存起来——保证同一对象在
// 上下文里期间只编码一次。
func (b *boundaryRecords) record(entry icodingagent.SessionEntry, pass uint64) json.RawMessage {
	message, ok := icodingagent.AsMessage(entry)
	if !ok {
		return entry.Raw()
	}
	m := message.Message
	key := boundaryRecordKey{id: message.ID, system: m.System, user: m.User, assistant: m.Assistant, toolResult: m.ToolResult}
	if key.system == nil && key.user == nil && key.assistant == nil && key.toolResult == nil {
		return entry.Raw()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if kept, ok := b.kept[key]; ok {
		b.kept[key] = keptBoundaryRecord{raw: kept.raw, pass: pass}
		return kept.raw
	}
	raw := entry.Raw()
	if b.kept == nil {
		b.kept = map[boundaryRecordKey]keptBoundaryRecord{}
	}
	b.kept[key] = keptBoundaryRecord{raw: raw, pass: pass}
	return raw
}

// forget drops the records pass did not hand out.
//
// 清掉本趟投影没碰过的记录——对应消息已滚出上下文，不再需要。
func (b *boundaryRecords) forget(pass uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for key, kept := range b.kept {
		if kept.pass != pass {
			delete(b.kept, key)
		}
	}
}
