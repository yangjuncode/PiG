package agent

import (
	"context"
	"slices"
	"sync/atomic"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/internal/jsloop"
)

// lifecycle.go 管理 Agent 的运行生命周期对外接口：事件订阅
// （Subscribe）、运行取消信号（Signal/Abort/WaitForIdle）、运行状态
// 派生（reduceEventState、StreamingMessage/PendingToolCalls/
// ErrorMessage）以及运行开始/结束时的观察者通知。
// Ports packages/agent/src/agent.ts.

// agentListener retains subscription identity independently of callback values.
type agentListener struct {
	callback func(context.Context, AgentEvent) error
	// observesToolUpdates reports whether the listener does anything with a tool_execution_update event; nil means it always may.
	observesToolUpdates func() bool
	// observesMessageUpdates reports whether the listener does anything with a message_update event; nil means it always may.
	observesMessageUpdates func() bool
	removed                atomic.Bool
}

// Subscribe registers a lifecycle listener. Listeners run in subscription order
// with the active run context; the run waits for each callback to return.
// The returned function removes this subscription and can be called repeatedly.
//
// 同步监听器注册：回调在 emit 里被逐个 await（同步调用），返回 error
// 会以 runFailure 终止本次运行——监听器不是"发了就忘"的，它参与运行
// 的控制流。注销走惰性删除：打 removed 标记，运行结束或下次非 streaming
// 状态时统一清扫，避免在事件派发中途改切片。
func (a *Agent) Subscribe(listener func(context.Context, AgentEvent) error) func() {
	return a.SubscribeObserving(listener, nil)
}

// SubscribeObserving is Subscribe for a listener that can say whether it currently does anything with a
// tool_execution_update event. observesToolUpdates is asked when a tool execution starts and again while it runs
// (ToolUpdatesObserved); nil means the listener always may. The listener still receives every event.
func (a *Agent) SubscribeObserving(listener func(context.Context, AgentEvent) error, observesToolUpdates func() bool) func() {
	return a.SubscribeObservingUpdates(listener, observesToolUpdates, nil)
}

// SubscribeObservingUpdates is SubscribeObserving for a listener that can also say whether it currently does anything
// with a message_update event. observesMessageUpdates is asked for every message_update; nil means the listener always
// may. While neither it nor anything else the Agent delivers events to may observe one (messageUpdatesObserved), the
// update is not delivered at all: no listener runs for it and the run does not await it. Once one may, every later update
// reaches every listener as before.
func (a *Agent) SubscribeObservingUpdates(listener func(context.Context, AgentEvent) error, observesToolUpdates, observesMessageUpdates func() bool) func() {
	entry := &agentListener{callback: listener, observesToolUpdates: observesToolUpdates, observesMessageUpdates: observesMessageUpdates}
	a.stateMu.Lock()
	a.listeners = append(a.listeners, entry)
	a.stateMu.Unlock()
	return func() {
		entry.removed.Store(true)
		a.stateMu.Lock()
		defer a.stateMu.Unlock()
		if !a.streaming {
			a.listeners = slices.DeleteFunc(a.listeners, func(listener *agentListener) bool { return listener.removed.Load() })
		}
	}
}

// toolUpdatesObserved reports whether anything the Agent delivers events to could see a tool_execution_update: the
// OnEvent, EventCh and AfterEvent options, or a live listener that may observe it.
func (a *Agent) toolUpdatesObserved() bool {
	if a.opts.OnEvent != nil || a.opts.EventCh != nil || a.opts.AfterEvent != nil {
		return true
	}
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	for _, listener := range a.listeners {
		if listener.removed.Load() {
			continue
		}
		if listener.observesToolUpdates == nil || listener.observesToolUpdates() {
			return true
		}
	}
	return false
}

// messageUpdatesObserved reports whether anything the Agent delivers events to could see a message_update now: the
// OnEvent, EventCh and AfterEvent options, or a live listener that may observe it. It is asked for every update, so a
// listener added or starting to observe during a stream sees every update from the next one on.
func (a *Agent) messageUpdatesObserved() bool {
	if a.opts.OnEvent != nil || a.opts.EventCh != nil || a.opts.AfterEvent != nil {
		return true
	}
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	for _, listener := range a.listeners {
		if listener.removed.Load() {
			continue
		}
		if listener.observesMessageUpdates == nil || listener.observesMessageUpdates() {
			return true
		}
	}
	return false
}

// runSignalWatcher retains an observer's identity independently of its callback value.
type runSignalWatcher struct {
	observe func()
	removed atomic.Bool
}

// ObserveRunSignal registers observe to run after a run claims the agent, when Signal already returns the run's context, and after the run releases it, when Signal already returns nil. Pi's `agent.signal` is a getter (agent.ts:336-338), so an owner that replicates the signal elsewhere learns of both instants here.
// observe runs on the goroutine that changes the run, after the agent's lock is released, so it may call Signal; the run waits for observe to return. The returned function removes the observer and can be called repeatedly.
func (a *Agent) ObserveRunSignal(observe func()) func() {
	watcher := &runSignalWatcher{observe: observe}
	a.stateMu.Lock()
	a.runSignalWatchers = append(a.runSignalWatchers, watcher)
	a.stateMu.Unlock()
	return func() {
		watcher.removed.Store(true)
		a.stateMu.Lock()
		defer a.stateMu.Unlock()
		a.runSignalWatchers = slices.DeleteFunc(a.runSignalWatchers, func(w *runSignalWatcher) bool { return w == watcher })
	}
}

// SetLoopTurns gives the agent the owner's model of Pi's event loop. The runs that start afterwards tell it when real work completes (a model response's I/O) and when a tool runs, so the owner can run the timer callbacks Pi's loop would run then. Pass nil to remove it.
func (a *Agent) SetLoopTurns(turns *jsloop.Turns) {
	a.stateMu.Lock()
	a.loopTurns = turns
	a.stateMu.Unlock()
}

// notifyRunSignalObservers runs the observers registered when the run changed, in registration order, skipping one removed meanwhile.
func (a *Agent) notifyRunSignalObservers() {
	a.stateMu.RLock()
	watchers := slices.Clone(a.runSignalWatchers)
	a.stateMu.RUnlock()
	for _, watcher := range watchers {
		if !watcher.removed.Load() {
			watcher.observe()
		}
	}
}

// Signal returns the active run's cancellation context, or nil when idle.
func (a *Agent) Signal() context.Context {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.runContext
}

// Abort cancels the active run. Calling Abort on an idle agent does nothing.
// 触发当前运行的取消（等价于上游 abort signal）：流迭代、工具执行、
// 钩子里的 ctx.Err() 都会看到。已结束的空闲 agent 调用是空操作。
func (a *Agent) Abort() {
	a.stateMu.RLock()
	cancel := a.runCancel
	a.stateMu.RUnlock()
	if cancel != nil {
		cancel()
	}
}

// WaitForIdle waits for the current run and its synchronous listeners to settle.
// It returns immediately when no run is active.
// 阻塞到本次运行彻底收尾（runDone 关闭于 releaseRun 末尾，即所有
// 状态清理完成之后）——用于"发了 Abort 后等它真的停稳"这类场景。
func (a *Agent) WaitForIdle() {
	a.stateMu.RLock()
	done := a.runDone
	a.stateMu.RUnlock()
	if done != nil {
		<-done
	}
}

// StreamingMessage returns a snapshot of the message currently streaming.
func (a *Agent) StreamingMessage() *AgentMessage {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if a.streamingMessage == nil {
		return nil
	}
	if a.streamingDeferred {
		// The last update was not delivered (reduceUnobservedUpdate): build the message it would have carried now, once.
		a.streamingStorage = AgentMessage{Assistant: shallowAssistantMessageFrom(&a.streamingCapture)}
		a.dropStreamingCapture()
	}
	message := startEventMessage(*a.streamingMessage)
	return &message
}

// reduceUnobservedUpdate is reduceEventState for a message_update that nothing observes: the state records the capture
// of the update's partial and StreamingMessage builds the message from it when asked, so the state reads exactly as if
// the update had been delivered.
func (a *Agent) reduceUnobservedUpdate(capture *ai.ShallowCapture) {
	a.stateMu.Lock()
	a.streamingCapture, a.streamingDeferred = *capture, true
	a.streamingMessage = &a.streamingStorage
	a.stateMu.Unlock()
}

// dropStreamingCapture forgets the capture of an undelivered update. The caller holds stateMu.
func (a *Agent) dropStreamingCapture() {
	if a.streamingDeferred {
		a.streamingCapture, a.streamingDeferred = ai.ShallowCapture{}, false
	}
}

// PendingToolCalls returns the active tool call IDs in start order.
func (a *Agent) PendingToolCalls() []string {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return slices.Clone(a.pendingToolCalls)
}

// ErrorMessage returns the last failed turn's error, cleared at the next run or reset.
func (a *Agent) ErrorMessage() string {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.errorMessage
}

// runDeadlineContext preserves the caller's deadline while cancellation forwarding
// is detached when the run settles. A successfully settled Pi signal is not aborted.
//
// 运行 ctx 的包装：active=true 期间把父 ctx 的取消透传出去（父取消 ⇒
// 运行取消）；运行落定后 active=false，只剩自身 cancel 状态——这样
// "父 ctx 恰好在运行成功结束的同时被取消"不会把已完成回合误标为
// aborted。Deadline 始终沿用调用方的，保证超时语义不丢。
type runDeadlineContext struct {
	context.Context
	parent      context.Context
	active      atomic.Bool
	deadline    time.Time
	hasDeadline bool
}

func (ctx *runDeadlineContext) Deadline() (time.Time, bool) { return ctx.deadline, ctx.hasDeadline }

func (ctx *runDeadlineContext) Err() error {
	if ctx.active.Load() {
		if err := ctx.parent.Err(); err != nil {
			return err
		}
	}
	return ctx.Context.Err()
}

// reduceEventState 把事件流折叠成可查询的瞬时状态：streamingMessage
// （当前流式中的消息快照）、pendingToolCalls（进行中的工具调用 ID）、
// errorMessage（最近失败回合的错误）。在 emit 内、监听器之前执行，
// 所以监听器看到的状态总是"当前事件已应用后"的值。
func (a *Agent) reduceEventState(ev AgentEvent) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	switch event := ev.(type) {
	case MessageStartEvent:
		a.streamingStorage = event.Message
		a.streamingMessage = &a.streamingStorage
		a.dropStreamingCapture()
	case MessageUpdateEvent:
		a.streamingStorage = event.Message
		a.streamingMessage = &a.streamingStorage
		a.dropStreamingCapture()
	case MessageEndEvent, AgentEndEvent:
		a.streamingMessage = nil
		a.streamingStorage = AgentMessage{}
		a.dropStreamingCapture()
	case ToolExecutionStartEvent:
		if !slices.Contains(a.pendingToolCalls, event.ToolCallID) {
			a.pendingToolCalls = append(a.pendingToolCalls, event.ToolCallID)
		}
	case ToolExecutionEndEvent:
		a.pendingToolCalls = slices.DeleteFunc(a.pendingToolCalls, func(id string) bool { return id == event.ToolCallID })
	case TurnEndEvent:
		if event.Message.Assistant != nil && event.Message.Assistant.ErrorMessage != "" {
			a.errorMessage = event.Message.Assistant.ErrorMessage
		}
	}
}

// notifyListeners 按注册顺序逐个同步调用监听器。每次取下一个监听器
// 都重新拿锁读下标——监听器回调可能注册新监听器（切片会变），按
// "每次重新读第 index 个"遍历就不会越界也不漏掉新加的。回调出错
// panic(runFailure) 终止运行，与上游 await 监听器抛异常同语义。
func (a *Agent) notifyListeners(ev AgentEvent) {
	for index := 0; ; index++ {
		a.stateMu.RLock()
		if index >= len(a.listeners) {
			a.stateMu.RUnlock()
			return
		}
		listener, ctx := a.listeners[index], a.runContext
		a.stateMu.RUnlock()
		if listener.removed.Load() {
			continue
		}
		if err := listener.callback(ctx, ev); err != nil {
			panic(runFailure{err: err})
		}
	}
}
