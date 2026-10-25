package coding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding/extension"
	"github.com/MichaelKinsy/PiG/coding/extension/host/invocation"
	icodingagent "github.com/MichaelKinsy/PiG/internal/codingagent"
)

// errAgentAlreadyProcessing is upstream prompt()'s error for a message sent
// while a run is active without a delivery mode.
var errAgentAlreadyProcessing = errors.New("Agent is already processing. Specify streamingBehavior ('steer' or 'followUp') to queue the message.")

// errPromptDuringCompaction is upstream prompt()'s error while compaction runs.
var errPromptDuringCompaction = errors.New("Cannot submit a prompt while compaction is in progress. Wait for compaction to finish and retry.")

// runtimeErrorPath is the ExtensionError path upstream uses for failures of
// session actions an extension started (agent-session.ts "<runtime>").
const runtimeErrorPath = "<runtime>"

// sessionRunState tracks the Session's agent run the way upstream
// AgentSession does with _isAgentRunActive, _agentRunAbortRequested,
// _isEmittingAgentSettled, _deferredSettledActions, and the idle-wait
// promise. Every mode reads streaming and idle state from here, and abort
// reaches the active run whichever caller started it.
//
// Session 层"本次运行"的完整状态机（区别于 agent 内部的 run
// 状态：这里把重试、压缩、排队续接都算进同一次运行）：
//   - active/settling/abortRequested：运行中 / 正在发 agent_settled /
//     用户请求了中止。
//   - generation + cancel：每次 beginAgentRun 递增代次并登记运行级
//     cancel，finishAgentRun 按代次校验——旧代次的清理函数碰不到
//     新代次的控制器。
//   - idle：WaitForIdle 的等待通道，每次状态翻转后关闭重置。
//   - deferred/pendingCustom：agent_settled 派发期间收到的 prompt 先
//     挂起，派发结束才放行（对应上游"settled 回调里的新 prompt 排队"）。
//   - background/bgCtx/bgCancel/shutdownOnce：扩展在空闲时自己起的
//     后台运行，Close 统一取消并等它们退出。
type sessionRunState struct {
	active         atomic.Bool
	settling       atomic.Bool
	abortRequested atomic.Bool
	// userRunsPending counts the runs an extension's sendUserMessage prepared or is preparing whose agent_start has not been emitted.
	userRunsPending atomic.Int32

	mu              sync.Mutex
	generation      uint64
	cancel          context.CancelFunc // cancels the active run
	isBeforeSettle  bool               // Abort preserves the awaited boundary response; protected by mu
	idle            chan struct{}      // closed when the session may have become idle
	deferred        []func()           // prompts sent while agent_settled was dispatched
	runningDeferred int                // deferred-prompt batches in progress, nested ones included; protected by mu
	deferredEpoch   uint64             // advances when an abort discards the deferred prompts; protected by mu
	pendingCustom   []agent.AgentMessage

	// Runs an extension starts while the session is idle are owned here:
	// Close cancels them and waits for them to return.
	background   sync.WaitGroup
	bgCtx        context.Context
	bgCancel     context.CancelFunc
	shutdownOnce sync.Once
}

// IsStreaming reports whether an agent run, including its retries,
// compaction, and queued continuations, is active (upstream isStreaming).
func (s *Session) IsStreaming() bool { return s.runState.active.Load() }

// Signal returns the active run's cancellation, or nil when no run is active. Extensions read it as ctx.signal.
//
// upstream: agent-session.ts:3368 (`getSignal: () => this.agent.signal`)
func (s *Session) Signal() context.Context { return s.agent.Signal() }

// ExtensionUserMessageRunPending reports whether a run an extension's sendUserMessage requested has not emitted agent_start yet. Pi's sendUserMessage goes through prompt(), which awaits the input handlers, the compaction check and before_agent_start before _startRun marks the Session busy, so the Session is still idle to the extension that sent it (agent-session.ts:2064-2140); here the run is prepared ahead of the extension's continuation and counts busy from its start (RunStart).
// 中文说明：扩展 sendUserMessage 触发的运行是否已备妥但还没发
// agent_start——上游此时 Session 对扩展仍"空闲"，这里自 RunStart
// 起就算忙（计数值>0）。
func (s *Session) ExtensionUserMessageRunPending() bool { return s.runState.userRunsPending.Load() > 0 }

// IsIdle reports whether the session has no active run and no compaction in
// flight (upstream isIdle).
func (s *Session) IsIdle() bool { return !s.IsStreaming() && !s.IsCompacting() }

// HasPendingMessages reports whether steering or follow-up messages are
// queued (upstream hasPendingMessages: pendingMessageCount > 0).
func (s *Session) HasPendingMessages() bool { return s.PendingMessageCount() > 0 }

// WaitForIdle blocks until the session is idle, ctx ends, or the session
// closes (upstream waitForIdle). A pending wait releases unawaited handler admission after its synchronous setup.
func (s *Session) WaitForIdle(ctx context.Context) error {
	if s.IsIdle() {
		return nil
	}
	for {
		s.runState.mu.Lock()
		// pig divergence (D142): a waiter registered while the Session was busy resolves only after agent_settled handlers and the prompts they deferred are done (Pi #10704).
		if s.IsIdle() && !s.settlementBusyLocked() {
			s.runState.mu.Unlock()
			return nil
		}
		if s.runState.idle == nil {
			s.runState.idle = make(chan struct{})
		}
		idle := s.runState.idle
		s.runState.mu.Unlock()
		// Upstream waitForIdle suspends at _getIdleWaitPromise; settlement needs the event executor released by this boundary.
		invocation.Acknowledge(ctx)
		select {
		case <-idle:
		case <-ctx.Done():
			return ctx.Err()
		case <-s.closeDone:
			return nil
		}
	}
}

// settlementBusyLocked reports whether agent_settled dispatch or a prompt it deferred is still pending. runState.mu must be held.
//
// "结算还在忙"的判定：agent_settled 正在派发、有被顺延的 prompt、
// 或顺延批次还在跑——三者有其一 idle 判定就不能放行。
func (s *Session) settlementBusyLocked() bool {
	return s.runState.settling.Load() || len(s.runState.deferred) > 0 || s.runState.runningDeferred > 0
}

// notifyIdleWaiters wakes WaitForIdle callers to re-check the idle state.
func (s *Session) notifyIdleWaiters() {
	s.runState.mu.Lock()
	if s.runState.idle != nil {
		close(s.runState.idle)
		s.runState.idle = nil
	}
	s.runState.mu.Unlock()
}

// RequestAbort stops Session continuations and cancels the Agent's signal without cancelling its caller context. An awaited before-settle hook still commits its drafts, but cannot continue. Extensions do not await the run they stop.
//
// 中止的完整扇面：打 abortRequested 标记（WillRetry 判定看的就是
// 它）→ 取消重试等待、压缩、分支摘要 → 取消 agent 当前运行。运行
// 不在 streaming（比如处于 before-settle 的扩展等待段）时改走
// runState.cancel——但 before-settle 阶段不直接取消，等钩子自己
// 收尾，只有 Session 关闭才强行取消。
func (s *Session) RequestAbort() { s.requestAbort(true) }

// requestAbort is RequestAbort. discardDeferred also drops the prompts agent_settled handlers deferred. Manual compaction keeps them: Pi's compact() aborts the run it replaces, not the continuations an extension queued.
func (s *Session) requestAbort(discardDeferred bool) {
	s.runState.mu.Lock()
	if discardDeferred && s.settlementBusyLocked() {
		// pig divergence (D142): an abort called while agent_settled settles discards the prompts already deferred, so it starts no new provider request (Pi #10705).
		s.runState.deferred = nil
		s.runState.deferredEpoch++
	}
	cancel := s.runState.cancel
	isBeforeSettle := s.runState.isBeforeSettle
	if s.runState.active.Load() {
		s.runState.abortRequested.Store(true)
	}
	s.runState.mu.Unlock()
	// Closing the Session still cancels its entire lifetime.
	select {
	case <-s.closeDone:
		isBeforeSettle = false
	default:
	}
	s.AbortRetry()
	s.AbortCompaction()
	s.AbortBranchSummary()
	if s.agent != nil && s.agent.IsStreaming() {
		s.agent.Abort()
	} else if cancel != nil && !isBeforeSettle {
		cancel()
	}
}

// AgentRunAbortRequested is upstream _agentRunAbortRequested: whether abort was requested while the current run was active. A new
// run clears it.
//
// 当前运行期间有没有人请求过 abort（上游 _agentRunAbortRequested）——
// 新运行清零。
func (s *Session) AgentRunAbortRequested() bool { return s.runState.abortRequested.Load() }

func (s *Session) agentRunAborted(ctx context.Context) bool {
	return ctx.Err() != nil || s.runState.abortRequested.Load()
}

// Abort cancels the active run, whoever started it, and waits until the
// session is idle (upstream abort).
func (s *Session) Abort(ctx context.Context) error {
	s.RequestAbort()
	return s.WaitForIdle(ctx)
}

// abortKeepingDeferred is Abort for manual compaction (upstream compact() awaits abort()).
//
// 手动压缩专用的 Abort：中止当前运行但不动顺延队列（上游
// compact() 内部的 await abort()）。
func (s *Session) abortKeepingDeferred(ctx context.Context) error {
	s.requestAbort(false)
	return s.WaitForIdle(ctx)
}

// beginAgentRun marks the run active and returns its cancellable context and
// owned cleanup (upstream _runAgentPrompt start). A stale run's cleanup cannot
// erase a newer run's cancellation.
func (s *Session) beginAgentRun(ctx context.Context) (context.Context, context.CancelFunc) {
	runCtx, cancel := context.WithCancel(ctx)
	return runCtx, s.ownAgentRun(cancel)
}

// ownAgentRun installs cancellation only after admission; a rejected claim must not replace another run's controller.
// 运行权注册：代次自增 + 登记 cancel + 清 abortRequested/active。
// 顺带做三项新运行的开场工作——清 failedResponse（上一轮压缩排的
// 重放被新 prompt 作废）、recordSelection、清 pendingToolNames（等
// 注册的迟到工具到下次运行就丢弃）。返回的清理闭包带代次校验。
// ownAgentRun installs the run RunAgentPrompt seeds, which no RunStart marked busy, so an abort requested before it no longer applies. It
// installs cancellation only after admission; a rejected claim must not replace another run's controller.
func (s *Session) ownAgentRun(cancel context.CancelFunc) context.CancelFunc {
	return s.installAgentRun(cancel, true)
}

// ownStartedRun installs the run of a RunStart once the Agent claimed it. An abort requested since the start still applies: _startRun
// clears the request only when the run starts, and _handleAgentEvent aborts the run at agent_start.
//
// RunStart 起跑后被 Agent 认领时的安装：与 ownAgentRun 的区别是
// 不清 abortRequested——start 到 agent_start 之间来的 abort 请求
// 依然有效，在 agent_start 处理点生效（同上游时序）。
func (s *Session) ownStartedRun(cancel context.CancelFunc) context.CancelFunc {
	return s.installAgentRun(cancel, false)
}

func (s *Session) installAgentRun(cancel context.CancelFunc, clearAbortRequest bool) context.CancelFunc {
	s.runState.mu.Lock()
	s.runState.generation++
	generation := s.runState.generation
	s.runState.cancel = cancel
	if clearAbortRequest {
		s.runState.abortRequested.Store(false)
	}
	s.runState.active.Store(true)
	s.runState.mu.Unlock()
	select {
	case <-s.closeDone:
		cancel()
	default:
	}
	// Compaction before the prompt may have scheduled a retry; the new prompt replaces it. upstream: agent-session.ts:1851-1856 (_startRun)
	s.failedResponse.Store(nil)
	s.recordSelection()
	s.clearPendingTools()
	return func() {
		s.failedResponse.Store(nil)
		cancel()
		s.finishAgentRun(generation)
	}
}

// RunStart is a run that counts as busy from its start (agent-session.ts:1826-1849 _startRun). While its compaction check and
// before_agent_start handlers run, the Session is streaming, so a message sent meanwhile joins the run and a prompt must name its
// streamingBehavior; bash results and custom messages wait for the run; and an abort requested meanwhile aborts the run at agent_start.
// It ends when the Agent claims the run, or with Abandon.
// RunStart 是一次"用户 prompt 起跑"的认领令牌：从标忙到 Agent
// 真正认领（或被 Abandon/settleUnclaimed 回收）之间，期间来的
// steering 消息归入本运行、bash 结果/custom 消息等待、abort 请求
// 在 agent_start 生效。generation 防"旧令牌误改新运行状态"。
type RunStart struct {
	session    *Session
	generation uint64
}

// BeginRunStart marks the Session busy for a user prompt's run (_startRun: `_isAgentRunActive = true; _agentRunAbortRequested = false`).
// A Session that is already busy rejects the prompt, as prompt() does when it is streaming (agent-session.ts:2098-2103).
// BeginRunStart 标忙（_startRun: `_isAgentRunActive = true`）：已关
// 会话报错、流式中拒新 prompt（与上游 prompt() 一致）。持
// pendingBashMu 让"忙判定"与其他 send/记录 bash 的决定原子化。
func (s *Session) BeginRunStart() (*RunStart, error) {
	s.pendingBashMu.Lock()
	defer s.pendingBashMu.Unlock()
	select {
	case <-s.closeDone:
		return nil, errors.New("coding: session is closed")
	default:
	}
	if s.IsStreaming() {
		return nil, errAgentAlreadyProcessing
	}
	return s.beginRunStartLocked(), nil
}

// beginRunStartLocked is BeginRunStart for a caller that holds pendingBashMu and found the Session idle, so the busy decision is atomic with
// the decisions of other sends and of recorded bash results.
// beginRunStartLocked 是已确认空闲且持 pendingBashMu 时的标忙：
// 代次自增、清 cancel/abortRequested、置 active——与 BeginRunStart
// 同一组原子操作。
func (s *Session) beginRunStartLocked() *RunStart {
	s.runState.mu.Lock()
	defer s.runState.mu.Unlock()
	s.runState.generation++
	s.runState.cancel = nil
	s.runState.abortRequested.Store(false)
	s.runState.active.Store(true)
	return &RunStart{session: s, generation: s.runState.generation}
}

// checkCompaction is _prepareRun's compaction check (agent-session.ts:1886-1891). Pi's check reports a failed or aborted compaction
// through compaction_end instead of rejecting, so after an abort the run still starts and stops at agent_start.
// checkCompaction 对应 _prepareRun 的压缩检查：压缩失败/中止经
// compaction_end 报告而不是拒绝运行——abort 之后运行照常开始，
// 到 agent_start 才停（上游时序）。
func (r *RunStart) checkCompaction(ctx context.Context) error {
	if err := r.session.CheckPromptCompaction(ctx); err != nil && !r.session.runState.abortRequested.Load() {
		return err
	}
	return nil
}

// FlushHeldMessages appends the bash results and custom messages held while the run was starting before its starting messages
// (agent-session.ts:1845-1848). No run exists yet, so they cannot split a tool call from its result. It reports whether it appended custom
// messages, whose message_start and message_end events precede preflightResult("started").
// FlushHeldMessages 把"起跑等待期间攒下的" bash 结果与 custom
// 消息在起始消息之前落盘——此刻还没有运行，不会把 tool_call 与
// 其结果拆开。返回值报是否追加了 custom 消息（它们要先发
// message_start/end 再发 preflightResult("started")）。
func (r *RunStart) FlushHeldMessages() (bool, error) {
	s := r.session
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runState.mu.Lock()
	custom := len(s.runState.pendingCustom) > 0
	s.runState.mu.Unlock()
	return custom, errors.Join(s.flushPendingBashLocked(), s.flushPendingCustomMessages())
}

// Abandon ends a start that failed before the Agent claimed its run (agent-session.ts:1838-1843, _startRun's catch): the Session returns
// to idle, what was held for the run is appended and idle waiters wake. A run that claimed the Session since then keeps its state.
// Abandon 回收一个"Agent 认领前就失败"的起跑：仅当代次仍归自己
// 时清状态回 idle、把攒下的消息落盘、跑顺延动作并唤醒等待者；
// 已被新运行认领的代次原样保留。
func (r *RunStart) Abandon() {
	s := r.session
	s.runState.mu.Lock()
	owned := s.runState.generation == r.generation
	if owned {
		s.runState.cancel = nil
		s.runState.active.Store(false)
	}
	s.runState.mu.Unlock()
	if !owned {
		return
	}
	s.mu.Lock()
	_ = s.flushPendingBashLocked()
	_ = s.flushPendingCustomMessages()
	s.mu.Unlock()
	s.runDeferredSettledActions()
}

// settleUnclaimed ends a start whose run the Agent rejected because another run holds it (agent-session.ts:1857-1876: agent.prompt
// throws inside _startRun's try, and its finally appends the held messages and emits agent_settled before the error propagates).
// settleUnclaimed 处理"运行被别人占着、Agent 拒收"的起跑：落盘
// 攒下消息、发 agent_settled、跑顺延动作（对应上游 _startRun 的
// try-finally 序列）。
func (r *RunStart) settleUnclaimed() {
	s := r.session
	s.mu.Lock()
	_ = s.flushPendingBashLocked()
	_ = s.flushPendingCustomMessages()
	s.mu.Unlock()
	s.emitAgentSettledNotification()
	s.runDeferredSettledActions()
}

func (s *Session) finishAgentRun(generation uint64) {
	s.runState.mu.Lock()
	if s.runState.generation != generation || !s.runState.active.Load() {
		s.runState.mu.Unlock()
		return
	}
	s.runState.cancel = nil
	s.runState.active.Store(false)
	idle := s.runState.idle
	s.runState.idle = nil
	s.runState.mu.Unlock()
	if idle != nil {
		close(idle)
	}
}

// endAgentRun ends a run whose caller owns agent_settled (RunAgentPrompt).
func (s *Session) endAgentRun(end context.CancelFunc) { end() }

// emitAgentSettled ends the run and emits agent_settled (upstream
// _emitAgentSettled). The run is inactive before extensions see the event,
// and a prompt an agent_settled handler sends is deferred until dispatch
// finishes. The call waits for extension dispatch and public subscribers, as upstream awaits _emitAgentSettled. Notifying the cache warmer is upstream
// _emitAgentSettled's first step (this._cacheWarmer?.onAgentSettled()),
// ahead of the event dispatch itself.
func (s *Session) emitAgentSettled() {
	s.OnAgentSettled()
	s.runState.mu.Lock()
	s.runState.cancel = nil
	s.runState.mu.Unlock()
	s.publishAgentSettled()
}

// A prompt rejected by the busy Agent still runs AgentSession's settled notification, but must not steal the running Agent's abort controller.
func (s *Session) emitAgentSettledNotification() {
	s.OnAgentSettled()
	s.publishAgentSettled()
}

func (s *Session) publishAgentSettled() {
	// 先翻 inactive 再派发——settled 监听器里调 IsStreaming 必须看到
	// 已结束；settling 标记让派发期间递进来的 prompt 改走 deferred。
	s.runState.active.Store(false)
	s.runState.settling.Store(true)
	defer s.runState.settling.Store(false)
	event := agent.AgentSettledEvent{Aborted: s.runState.abortRequested.Load()}
	// Session-owned handler effects must reach the output funnel before the settled event itself.
	s.dispatchAgentEventToExtensions(event)
	s.emitOrderedEventSync(event)
}

// runDeferredSettledActions runs the prompts deferred during agent_settled,
// in order, and then wakes idle waiters (upstream _emitAgentSettled tail).
// A deferred prompt's own run settles through a nested call; the batch count keeps the outer batch's later prompts pending for idle waiters and aborts.
func (s *Session) runDeferredSettledActions() {
	for s.runDeferredSettledBatch() {
	}
	s.notifyIdleWaiters()
}

// runDeferredSettledBatch runs the prompts deferred so far and reports whether there were any.
// runDeferredSettledBatch 取走当前顺延批（deferred 清槽、记 epoch、
// 计 runningDeferred）并逐个跑——返回"本批是否有事"。
func (s *Session) runDeferredSettledBatch() bool {
	s.runState.mu.Lock()
	deferred := s.runState.deferred
	s.runState.deferred = nil
	epoch := s.runState.deferredEpoch
	if len(deferred) > 0 {
		s.runState.runningDeferred++
	}
	s.runState.mu.Unlock()
	if len(deferred) == 0 {
		return false
	}
	defer func() {
		s.runState.mu.Lock()
		s.runState.runningDeferred--
		s.runState.mu.Unlock()
	}()
	for _, action := range deferred {
		s.runState.mu.Lock()
		discarded := s.runState.deferredEpoch != epoch
		s.runState.mu.Unlock()
		if discarded {
			break
		}
		action()
	}
	return true
}

// RunInputHandlers runs the extension input handlers for user input and
// returns the text and images to use, or handled when an extension consumed
// it (upstream _runInputHandlers). behavior ("steer" or "followUp") reaches
// the handlers only while a run is active, as upstream passes it only when
// streaming. A transform without images keeps the original images.
func (s *Session) RunInputHandlers(ctx context.Context, text string, images []ai.ImageContent, source extension.InputSource, behavior string) (string, []ai.ImageContent, bool, error) {
	if !s.IsStreaming() {
		behavior = ""
	}
	return icodingagent.RunInputHandlers(ctx, s.currentRunner(), text, images, source, behavior)
}

// backgroundContext is the lifetime of runs the session starts itself.
func (s *Session) backgroundContext() context.Context {
	s.runState.mu.Lock()
	defer s.runState.mu.Unlock()
	if s.runState.bgCtx == nil {
		s.runState.bgCtx, s.runState.bgCancel = context.WithCancel(context.Background())
	}
	return s.runState.bgCtx
}

// startExtensionTask registers work before shutdown can begin its join.
func (s *Session) startExtensionTask(work func()) error {
	s.runState.mu.Lock()
	defer s.runState.mu.Unlock()
	select {
	case <-s.closeDone:
		return errors.New("coding: session is closed")
	default:
	}
	s.runState.background.Go(work)
	return nil
}

// shutdownRuns cancels the runs the session started itself and waits for
// them. Close calls it after the event funnel closes, so none can block.
func (s *Session) shutdownRuns() {
	s.runState.shutdownOnce.Do(func() {
		s.runState.mu.Lock()
		if s.runState.bgCancel != nil {
			s.runState.bgCancel()
		} else {
			s.runState.bgCtx, s.runState.bgCancel = context.WithCancel(context.Background())
			s.runState.bgCancel()
		}
		s.runState.mu.Unlock()
		s.runState.background.Wait()
	})
}

// reportRuntimeError reports a failed extension-started action through the
// extension error listeners, as upstream's runner.emitError with
// extensionPath "<runtime>".
func (s *Session) reportRuntimeError(event string, err error) {
	if err == nil {
		return
	}
	if runner := s.currentRunner(); runner != nil {
		runner.EmitError(&extension.ExtensionError{ExtensionPath: runtimeErrorPath, Event: event, Error: err.Error()})
	}
}

func ignoreCancellation(err error) error {
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// extensionUserMessageContent normalizes sendUserMessage content: a string,
// or text and image blocks whose texts join with "\n" (upstream
// sendUserMessage).
func extensionUserMessageContent(raw any) (string, []ai.ImageContent, error) {
	var blocks []ai.UserContentBlock
	switch value := raw.(type) {
	case string:
		return value, nil, nil
	case ai.UserText:
		return string(value), nil, nil
	case ai.UserContentBlocks:
		blocks = value
	case []ai.UserContentBlock:
		blocks = value
	default:
		encoded, err := json.Marshal(raw)
		if err != nil {
			return "", nil, fmt.Errorf("user content must be a string or array: %w", err)
		}
		var wire []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Data     string `json:"data"`
			MimeType string `json:"mimeType"`
		}
		if err := json.Unmarshal(encoded, &wire); err != nil {
			return "", nil, fmt.Errorf("user content must be a string or array")
		}
		for i, block := range wire {
			switch block.Type {
			case "text":
				blocks = append(blocks, ai.TextContent{Text: block.Text})
			case "image":
				blocks = append(blocks, ai.ImageContent{Data: block.Data, MimeType: block.MimeType})
			default:
				return "", nil, fmt.Errorf("user content[%d] has unsupported type %q", i, block.Type)
			}
		}
	}
	var texts []string
	var images []ai.ImageContent
	for _, block := range blocks {
		switch block := block.(type) {
		case ai.TextContent:
			texts = append(texts, block.Text)
		case ai.ImageContent:
			images = append(images, block)
		}
	}
	return strings.Join(texts, "\n"), images, nil
}
