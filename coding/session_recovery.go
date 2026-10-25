package coding

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	icodingagent "github.com/MichaelKinsy/PiG/internal/codingagent"
	"github.com/MichaelKinsy/PiG/internal/codingagent/compaction"
)

// messageIdentity returns the pointer that identifies one in-memory message,
// or nil for messages without one (custom messages).
func messageIdentity(message agent.AgentMessage) any {
	switch {
	case message.System != nil:
		return message.System
	case message.User != nil:
		return message.User
	case message.Assistant != nil:
		return message.Assistant
	case message.ToolResult != nil:
		return message.ToolResult
	}
	return nil
}

// rememberMessageEntry records the Session entry that persisted message.
func (s *Session) rememberMessageEntry(message agent.AgentMessage, entryID string) {
	key := messageIdentity(message)
	if key == nil || entryID == "" {
		return
	}
	s.entryIDsMu.Lock()
	if s.entryIDsByMessage == nil {
		s.entryIDsByMessage = make(map[any]string)
	}
	s.entryIDsByMessage[key] = entryID
	s.entryIDsMu.Unlock()
}

// refreshContext replaces the agent's finalized transcript with the canonical
// Session projection and re-indexes each projected message by its source
// entry (agent-session.ts _refreshFinalizedContext). The caller serializes
// with the agent loop.
func (s *Session) refreshContext() {
	s.refreshProjectedContext(true)
}

// refreshReplacementContext installs only the incoming Session's canonical
// projection. An outgoing instruction baseline belongs to the old Session and
// must not cross a resume or switch boundary.
func (s *Session) refreshReplacementContext() {
	s.refreshProjectedContext(false)
}

// refreshProjectedContext 是"磁盘为准"的上下文重建：用 JSONL 投影
// （BuildSessionProjection，含压缩/编辑/分支的最新语义）整体替换
// agent 内存转录，并重建消息→条目 ID 映射。投影生成的是全新消息
// 对象，所以刚结束回合的消息要靠 lastAssistant* 指针把旧 ID 映射
// 带过来。preserveInstructionBaseline=false 用于换底（ReplaceInner）：
// 旧会话的系统提示基线不带过界。
func (s *Session) refreshProjectedContext(preserveInstructionBaseline bool) {
	// The agent's transcript shares the messages of the kept projection, as the requests do: the history is held once, not once for the agent and once for the projection.
	projection := s.inner.BuildSessionProjectionView()
	ids := make(map[any]string, len(projection.Messages))
	for _, entry := range projection.Entries {
		for _, message := range entry.Messages {
			if key := messageIdentity(message); key != nil {
				ids[key] = entry.SourceEntry.Base().ID
			}
		}
	}
	s.entryIDsMu.Lock()
	// Projection creates new message objects. Preserve the observed turn's IDs only until post-run recovery consumes it.
	if key := messageIdentity(agent.AgentMessage{Assistant: s.lastAssistantMessage}); key != nil {
		if id, found := s.entryIDsByMessage[key]; found {
			ids[key] = id
		}
	}
	s.entryIDsByMessage = ids
	s.entryIDsMu.Unlock()
	if preserveInstructionBaseline {
		restoreAgentMessages(s.agent, projection.Messages)
		return
	}
	s.agent.SetMessages(projection.Messages)
}

// RefreshContext refreshes the agent's finalized transcript from the canonical
// Session projection (agent-session.ts refreshContext). Call it after
// appending entries to Inner() directly.
func (s *Session) RefreshContext() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshContext()
}

// findPersistedMessageEntryID resolves the Session entry that persisted an
// in-memory message (agent-session.ts _findPersistedMessageEntryId). A
// message not indexed at persistence or refresh time resolves through its
// position in the agent transcript, which follows the projection.
func (s *Session) findPersistedMessageEntryID(message agent.AgentMessage) (string, bool) {
	key := messageIdentity(message)
	if key == nil {
		return "", false
	}
	s.entryIDsMu.Lock()
	id, found := s.entryIDsByMessage[key]
	s.entryIDsMu.Unlock()
	if found {
		return id, true
	}
	if message.ToolResult != nil {
		// 快路径：toolResult 有天然 ID（ToolCallID），直接从投影倒查。
		// The newest projected entries are the turn's own: the kept projection extends by each append, so the lookup reads them without projecting the history, and a match ends the walk.
		projection := s.inner.BuildSessionProjectionView()
		for _, entry := range slices.Backward(projection.Entries) {
			for _, projected := range entry.Messages {
				if projected.ToolResult != nil && projected.ToolResult.ToolCallID == message.ToolResult.ToolCallID {
					s.rememberMessageEntry(message, entry.SourceEntry.Base().ID)
					return entry.SourceEntry.Base().ID, true
				}
			}
		}
	}
	// 慢路径：按消息在转录里的位置去投影里数——投影顺序和转录一致。
	// 转录开头若多了一条"恢复时注入的基线 system"（投影里没有），
	// 位置要先-1 对齐。
	messageIndex := slices.IndexFunc(s.agent.Messages(), func(candidate agent.AgentMessage) bool {
		return messageIdentity(candidate) == key
	})
	if messageIndex < 0 {
		return "", false
	}
	projection := s.inner.BuildSessionProjectionView()
	// Restoring a transcript without a system entry prepends the current
	// baseline. It has no persisted entry and shifts projected positions.
	messages := s.agent.Messages()
	if len(messages) > 0 && messages[0].System != nil && (len(projection.Messages) == 0 || projection.Messages[0].System == nil) {
		messageIndex--
	}
	if messageIndex < 0 {
		return "", false
	}
	projectedIndex := 0
	for _, entry := range projection.Entries {
		for _, projected := range entry.Messages {
			if projectedIndex == messageIndex {
				if projected.Role() != message.Role() {
					return "", false
				}
				s.rememberMessageEntry(message, entry.SourceEntry.Base().ID)
				return entry.SourceEntry.Base().ID, true
			}
			projectedIndex++
		}
	}
	return "", false
}

// isInAgentTranscript reports whether message is one of the agent's current
// in-memory messages.
func (s *Session) isInAgentTranscript(message agent.AgentMessage) bool {
	key := messageIdentity(message)
	return key != nil && slices.ContainsFunc(s.agent.Messages(), func(candidate agent.AgentMessage) bool {
		return messageIdentity(candidate) == key
	})
}

var errUnresolvedRecoveryTarget = errors.New("Cannot persist recovery omission because a projected message has no source entry")

// omitRecoveryAttempt durably omits a failed final attempt and its tool
// results from model context with context_edit entries, keeping them in raw
// history (agent-session.ts _omitRecoveryAttempt). The tool results are found
// in the agent transcript itself, so the pair stays together whether the
// attempt comes from the current run or a resumed session (Pi #11227): an
// unpaired result fails the next request.
func (s *Session) omitRecoveryAttempt(message *agent.AssistantMessage) error {
	// 先收集这次失败尝试发出的全部 toolCall ID——它的工具
	// 结果必须跟它一起抠掉，否则孤儿 result 会破坏下一次
	// 请求的 tool_call/tool_result 配对校验。
	toolCallIDs := make(map[string]struct{})
	for _, block := range message.Content {
		if call, ok := block.(ai.ToolCall); ok {
			toolCallIDs[call.ID] = struct{}{}
		}
	}
	// A context edit replaces the projected copy, so match it by source entry as well.
	// 定位 assistant 消息在转录中的位置：指针优先，context edit
	// 换过投影副本时按同一 source entry 兜底匹配。
	entryID, entryFound := s.findPersistedMessageEntryID(agent.AgentMessage{Assistant: message})
	messages := s.agent.Messages()
	index := slices.IndexFunc(messages, func(m agent.AgentMessage) bool {
		if m.Assistant == message {
			return true
		}
		if !entryFound {
			return false
		}
		key := messageIdentity(m)
		if key == nil {
			return false
		}
		s.entryIDsMu.Lock()
		id, found := s.entryIDsByMessage[key]
		s.entryIDsMu.Unlock()
		return found && id == entryID
	})
	// 从定位点向后收配对 tool result：遇到下一条 assistant
	// 消息即停——之后的 result 属于别的尝试。
	targets := []agent.AgentMessage{{Assistant: message}}
	if index >= 0 {
		for _, m := range messages[index+1:] {
			if m.Assistant != nil {
				break
			}
			if m.ToolResult != nil {
				if _, paired := toolCallIDs[m.ToolResult.ToolCallID]; paired {
					targets = append(targets, m)
				}
			}
		}
	}
	targetIDs := make([]string, len(targets))
	for i, target := range targets {
		id, found := s.findPersistedMessageEntryID(target)
		if !found && s.isInAgentTranscript(target) {
			return errUnresolvedRecoveryTarget
		}
		targetIDs[i] = id
	}
	for _, targetID := range targetIDs {
		if targetID == "" {
			continue
		}
		editID, err := s.inner.AppendContextEdit(targetID, nil)
		if err != nil {
			return err
		}
		if entry, ok := s.inner.GetEntry(editID); ok {
			s.emitOrderedEvent(agent.EntryAppendedEvent{Entry: entry.Raw()})
		}
	}
	s.refreshContext()
	return nil
}

// runPostAgentRuns repeats post-run handling and continuations until the run settles (agent-session.ts _runAgentPrompt loop). Session abort and caller cancellation both stop continuation. The caller holds s.mu.
func (s *Session) runPostAgentRuns(ctx context.Context, messages []agent.AgentMessage, runErr error) ([]agent.AgentMessage, error) {
	return s.runPostAgentRunsWith(ctx, messages, runErr, func(ctx context.Context) ([]agent.AgentMessage, error) {
		s.mu.Unlock()
		defer s.mu.Lock()
		return s.agent.ContinueMessages(ctx)
	})
}

// runPostAgentRunsWith is runPostAgentRuns with the continuation supplied by
// the caller, which decides whether the Session lock is held across it.
//
// 跑后处理主循环：每次底层运行结束后先过 handlePostAgentRun（重试/
// 溢出恢复/压缩裁决要不要续），不需要续时再给 before_settle 扩展
// 边界一次机会（它可注入消息要求续）。要续就调用方提供的 resume
// 再来一轮。任一步发现已中止就退出并收尾被砍掉的重试序列。
func (s *Session) runPostAgentRunsWith(ctx context.Context, messages []agent.AgentMessage, runErr error, resume func(context.Context) ([]agent.AgentMessage, error)) ([]agent.AgentMessage, error) {
	for !s.agentRunAborted(ctx) {
		continueRun, err := s.handlePostAgentRun(ctx)
		if err != nil {
			return messages, err
		}
		if !continueRun && !s.agentRunAborted(ctx) {
			continueRun, err = s.runBeforeSettleBoundary(ctx)
			if err != nil {
				return messages, err
			}
		}
		if !continueRun || s.agentRunAborted(ctx) {
			break
		}
		messages, runErr = resume(ctx)
	}
	if s.agentRunAborted(ctx) {
		s.finishCancelledRetry()
	}
	return messages, runErr
}

// handlePostAgentRun applies retry, recovery, and compaction to one finished
// low-level run and reports whether the agent should continue
// (agent-session.ts _handlePostAgentRun).
// handlePostAgentRun 对刚结束的一次底层运行做"续不续"裁决，按序：
// 已中止→收尾退出；无 assistant 产出（比如队列被清空）→看队列有
// 没有存粮；可重试错误→走指数退避重试（并把失败回复存
// failedResponse 供重放）；不可重试但仍 error→结束重试序列并发
// AutoRetryEnd 失败版；然后过压缩检查（溢出恢复/阈值压缩可要求
// 续跑）；最后队列里有消息才续。
func (s *Session) handlePostAgentRun(ctx context.Context) (bool, error) {
	message := s.lastAssistantMessage
	s.lastAssistantMessage = nil
	if s.agentRunAborted(ctx) {
		s.finishCancelledRetry()
		return false, nil
	}
	if message == nil {
		return s.agent.HasQueuedMessages(), nil
	}
	if icodingagent.IsRetryableError(message, s.contextWindowFor(message)) {
		retrying, err := s.prepareRetry(ctx, message)
		if err != nil {
			return false, err
		}
		if retrying {
			s.failedResponse.Store(message)
			if s.agentRunAborted(ctx) {
				s.finishCancelledRetry()
			}
			return !s.agentRunAborted(ctx), nil
		}
	}
	if s.agentRunAborted(ctx) {
		s.finishCancelledRetry()
		return false, nil
	}
	if message.StopReason == ai.StopReasonError {
		// 走到这里说明错误不可重试或已超限：序列结束，补发
		// auto_retry_end(success=false)。
		if attempt := s.retryAttempt.Swap(0); attempt > 0 {
			s.emitEvent(agent.AutoRetryEndEvent{Success: false, Attempt: int(attempt), FinalError: message.ErrorMessage})
		}
	}
	compacted, err := s.checkCompaction(ctx, message, true)
	if err != nil {
		return false, err
	}
	if compacted {
		return !s.agentRunAborted(ctx), nil
	}
	return !s.agentRunAborted(ctx) && s.agent.HasQueuedMessages(), nil
}

// prepareRetry schedules one automatic retry of a retryable error: it emits
// auto_retry_start, omits the failed attempt, and waits out the backoff
// (agent-session.ts _prepareRetry). It reports false when retries are
// disabled or exhausted, or when the wait is cancelled.
// prepareRetry 的一次重试流程：retryPrefix 的临界区里先判"还能不
// 能重试"（开关/上限/已中止），能则递增计数、算退避延迟、发
// auto_retry_start、把失败回复从上下文抠掉（omitRecoveryAttempt），
// 再建可取消的等待器；临界区外放开 s.mu 等计时器/AbortRetry 唤醒。
// 等待期间 s.mu 是释放的——其他 Session 操作可以照常进行。
func (s *Session) prepareRetry(ctx context.Context, message *agent.AssistantMessage) (bool, error) {
	s.retryMu.Lock()
	schedule := s.retrySchedule
	s.retryMu.Unlock()
	wait, err := s.retryPrefix(schedule, func() (*sessionRetryWait, error) {
		cfg := s.SettingsManager().GetRetrySettings()
		if !cfg.Enabled || s.agentRunAborted(ctx) {
			return nil, nil
		}
		attempt := int(s.retryAttempt.Add(1))
		if attempt > cfg.MaxRetries {
			s.retryAttempt.Add(-1)
			return nil, nil
		}
		delayMs := ai.RetryDelayMs(ai.RetryPolicy{BaseDelayMs: cfg.BaseDelayMs, MaxAgentDelayMs: &cfg.MaxAgentDelayMs}, attempt)
		errorMessage := message.ErrorMessage
		if errorMessage == "" {
			errorMessage = "Unknown error"
		}
		retryCtx, cancel := context.WithCancel(ctx)
		wait := &sessionRetryWait{session: s, ctx: retryCtx, cancel: cancel, schedule: schedule, done: make(chan retryWaitResult, 1)}
		// Publish the cancellation owner before another Go goroutine can observe auto_retry_start. The queued host prefix still emits start and omission in Pi's order.
		s.retryMu.Lock()
		s.retryCancel = wait
		s.retryMu.Unlock()
		s.emitEvent(agent.AutoRetryStartEvent{Attempt: attempt, MaxAttempts: cfg.MaxRetries, DelayMs: delayMs, ErrorMessage: errorMessage})
		if err := s.omitRecoveryAttempt(message); err != nil {
			cancel()
			s.retryMu.Lock()
			if s.retryCancel == wait {
				s.retryCancel = nil
			}
			s.retryMu.Unlock()
			return nil, err
		}
		wait.timer = time.NewTimer(time.Duration(delayMs) * time.Millisecond)
		return wait, nil
	})
	if err != nil || wait == nil {
		return false, err
	}
	defer wait.cancel()
	defer wait.timer.Stop()
	s.mu.Unlock()
	retry, err := wait.await()
	s.mu.Lock()
	return retry, err
}

// finishCancelledRetry ends an in-progress retry sequence after an abort.
func (s *Session) finishCancelledRetry() {
	if attempt := s.retryAttempt.Swap(0); attempt > 0 {
		s.emitEvent(agent.AutoRetryEndEvent{Success: false, Attempt: int(attempt), FinalError: "Retry cancelled"})
	}
}

// assistantEndNote records the agent-goroutine decisions for one assistant
// message_end: whether the run would retry it if it ends the run, and the
// attempt count of a retry sequence it ended successfully (zero if none).
type assistantEndNote struct {
	willRetry  bool
	retryEnded int32
}

// noteAssistantMessageEnd applies Pi's assistant message_end bookkeeping: a
// response that is neither an error nor a length stop re-arms overflow
// recovery, and a non-error response ends an automatic retry sequence. It runs
// on the agent goroutine when the message persists, before post-run handling
// can commit another retry.
func (s *Session) noteAssistantMessageEnd(message *agent.AssistantMessage) assistantEndNote {
	note := assistantEndNote{willRetry: s.willRetryAfterAgentEnd(message)}
	// 两个簿记在 agent goroutine 上、跑后裁决之前完成：
	// 非 error/length 的成功回复重置溢出恢复防重入标记；
	// 非 error 回复终止当前重试序列（计数换出留作事件用）。
	if message.StopReason != ai.StopReasonError && message.StopReason != ai.StopReasonLength {
		s.overflowRecoveryAttempted.Store(false)
	}
	if message.StopReason != ai.StopReasonError {
		note.retryEnded = s.retryAttempt.Swap(0)
	}
	return note
}

func (s *Session) pushAssistantEnd(note assistantEndNote) {
	s.assistantEndsMu.Lock()
	s.assistantEnds = append(s.assistantEnds, note)
	s.assistantEndsMu.Unlock()
}

// popAssistantEnd returns the note for the next forwarded assistant
// message_end. Messages that never persisted have no note.
func (s *Session) popAssistantEnd() assistantEndNote {
	s.assistantEndsMu.Lock()
	defer s.assistantEndsMu.Unlock()
	if len(s.assistantEnds) == 0 {
		return assistantEndNote{}
	}
	note := s.assistantEnds[0]
	s.assistantEnds = s.assistantEnds[1:]
	return note
}

// contextWindowFor is the context window that applies to message: the window of the model that produced it, else of the selected model.
//
// upstream: agent-session.ts:3613 (_isRetryableError)
func (s *Session) contextWindowFor(message *agent.AssistantMessage) int {
	model := s.modelForMessage(message)
	if model == nil {
		model = s.Model()
	}
	if model == nil {
		return 0
	}
	return model.Capabilities.ContextWindow
}

func (s *Session) contextWindow() int {
	model := s.Model()
	if model == nil {
		return 0
	}
	return model.Capabilities.ContextWindow
}

func (s *Session) currentBranch() []icodingagent.SessionEntry {
	return s.inner.GetContextBranch()
}

// compactionSettings returns the compaction settings for the current model,
// with its compaction.modelOverrides entry applied (upstream
// getCompactionSettings(this.model)).
func (s *Session) compactionSettings() (compaction.CompactionSettings, error) {
	cfg, err := s.SettingsManager().GetCompactionSettings(s.Model())
	if err != nil {
		return compaction.CompactionSettings{}, err
	}
	return compaction.CompactionSettings{
		Enabled:          cfg.Enabled,
		ReserveTokens:    cfg.ReserveTokens,
		KeepRecentTokens: cfg.KeepRecentTokens,
	}, nil
}

func estimateMessagesTokens(messages []agent.AgentMessage) int {
	tokens := 0
	for _, message := range messages {
		tokens += compaction.EstimateTokens(message)
	}
	return tokens
}
