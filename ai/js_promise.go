// 中文导读：ECMAScript Promise / async / await 的 Go 建模。
//
// 上游 provider、stream 与 agent 代码里每个 await 的恢复顺序由
// V8 微任务队列决定。本文件在 Go 里复刻同一机制：jsPromise、
// jsResolvers、jsCoroutine 只能在 executor 的 turn/reaction/协程体
// 内使用；executor 的 FIFO ready 队列就是微任务队列，反应永不
// 内联执行。写在其上的 provider/流循环花掉与 V8 相同的 reaction
// 数——这是事件交付时序与上游一致的根基。
package ai

import (
	"errors"
	"iter"
)

// This file models the ECMAScript Promise, async function and await constructs that Pi's provider, stream and agent code awaits. A provider or stream loop written on it spends the microtask reactions V8 spends at every await.
//
// The model is single threaded. Every jsPromise, jsResolvers and jsCoroutine must be used only by code that runs on its executor: a turn body, a reaction, or a coroutine body that a reaction or turn resumed. The executor serializes those, and its FIFO ready queue is the microtask queue. Reactions never run inline; a reaction posted while another runs joins the tail.
//
// Ports (ECMA-262 2025):
//   - 27.2.1.3 CreateResolvingFunctions, 27.2.1.4 FulfillPromise, 27.2.1.7 RejectPromise, 27.2.1.8 TriggerPromiseReactions
//   - 27.2.2.1 NewPromiseReactionJob, 27.2.2.2 NewPromiseResolveThenableJob
//   - 27.2.5.4.1 PerformPromiseThen, 27.2.4.7.1 PromiseResolve
//   - 6.2.3.1 Await, 27.7.5.1 AsyncFunctionStart
//
// Await consumes one reaction for a native promise and for a plain value, and three (a thenable job, the thenable's own reaction, then the await reaction) for a non-native thenable; V8 has skipped the extra wrapper promise for native promises since 7.2, which the specification adopted in 2018.

type jsPromiseState uint8

const (
	jsStatePending jsPromiseState = iota
	jsStateFulfilled
	jsStateRejected
)

// jsThenable is an object with a callable then property. then reports a thrown exception as its error.
type jsThenable[T any] interface {
	then(resolve func(jsOperand[T]), reject func(error)) error
}

// jsFuncThenable adapts a function to jsThenable for foreign thenables such as SDK helper objects.
type jsFuncThenable[T any] func(resolve func(jsOperand[T]), reject func(error)) error

func (f jsFuncThenable[T]) then(resolve func(jsOperand[T]), reject func(error)) error {
	return f(resolve, reject)
}

// jsOperand is a JavaScript value in a position that treats promises and thenables specially: an argument to resolve, an await operand, a return operand or a yield operand. Exactly one of value, promise or thenable applies. A native promise is also its own thenable.
type jsOperand[T any] struct {
	value    T
	thenable jsThenable[T]
	promise  *jsPromise[T]
}

func jsValue[T any](value T) jsOperand[T] { return jsOperand[T]{value: value} }

func jsPromiseOperand[T any](promise *jsPromise[T]) jsOperand[T] {
	return jsOperand[T]{promise: promise, thenable: promise}
}

func jsThenableOperand[T any](thenable jsThenable[T]) jsOperand[T] {
	return jsOperand[T]{thenable: thenable}
}

type jsReaction[T any] struct {
	fulfilled func(T)
	rejected  func(error)
}

// jsPromise is a native Promise whose reaction jobs run on its executor.
type jsPromise[T any] struct {
	executor  *continuationExecutor
	state     jsPromiseState
	value     T
	reason    error
	reactions []jsReaction[T]
	resolvers *jsResolvers[T]
}

func newJSPromise[T any](executor *continuationExecutor) *jsPromise[T] {
	promise := &jsPromise[T]{executor: executor}
	promise.resolvers = promise.newResolvers()
	return promise
}

// jsResolved is Promise.resolve(value) for a non-promise value.
func jsResolved[T any](executor *continuationExecutor, value T) *jsPromise[T] {
	promise := newJSPromise[T](executor)
	promise.resolve(jsValue(value))
	return promise
}

// jsRejected is Promise.reject(reason).
func jsRejected[T any](executor *continuationExecutor, reason error) *jsPromise[T] {
	promise := newJSPromise[T](executor)
	promise.reject(reason)
	return promise
}

// resolve is the resolve function passed to a new Promise's executor. Only the first call to resolve or reject has an effect.
func (promise *jsPromise[T]) resolve(operand jsOperand[T]) { promise.resolvers.resolve(operand) }

// reject is the reject function passed to a new Promise's executor.
func (promise *jsPromise[T]) reject(reason error) { promise.resolvers.reject(reason) }

// jsResolvers is one pair of resolving functions with its shared alreadyResolved flag.
type jsResolvers[T any] struct {
	promise *jsPromise[T]
	already bool
}

func (promise *jsPromise[T]) newResolvers() *jsResolvers[T] {
	return &jsResolvers[T]{promise: promise}
}

// errJSChainingCycle is the TypeError for resolving a promise with itself.
var errJSChainingCycle = errors.New("Chaining cycle detected for promise #<Promise>")

func (resolvers *jsResolvers[T]) resolve(operand jsOperand[T]) {
	// alreadyResolved：spec 的 [[AlreadyResolved]]——每对
	// resolving functions 只有第一个调用生效。
	if resolvers.already {
		return
	}
	resolvers.already = true
	promise := resolvers.promise
	// 用自身 resolve 是 TypeError（链式环检测）。
	if operand.promise == promise {
		promise.settleRejected(errJSChainingCycle)
		return
	}
	// 普通值：直接 fulfill；thenable（含原生 promise）：不能同步
	// 收养，必须排一个 PromiseResolveThenableJob 微任务——这正
	// 是 await 一个 thenable 花 3 个 reaction 的原因。
	if operand.thenable == nil {
		promise.settleFulfilled(operand.value)
		return
	}
	thenable := operand.thenable
	promise.executor.post(func() {
		pair := promise.newResolvers()
		if err := thenable.then(pair.resolve, pair.reject); err != nil {
			pair.reject(err)
		}
	})
}

func (resolvers *jsResolvers[T]) reject(reason error) {
	if resolvers.already {
		return
	}
	resolvers.already = true
	resolvers.promise.settleRejected(reason)
}

func (promise *jsPromise[T]) settleFulfilled(value T) {
	// TriggerPromiseReactions：先按注册顺序取出 reactions 并清空
	// （settle 是一次性的），再逐条 post 到微任务队列——反应永远
	// 异步执行，哪怕 promise 已 settle。
	reactions := promise.reactions
	promise.reactions = nil
	promise.state = jsStateFulfilled
	promise.value = value
	for _, reaction := range reactions {
		promise.executor.post(func() { reaction.fulfilled(value) })
	}
}

func (promise *jsPromise[T]) settleRejected(reason error) {
	reactions := promise.reactions
	promise.reactions = nil
	promise.state = jsStateRejected
	promise.reason = reason
	for _, reaction := range reactions {
		promise.executor.post(func() { reaction.rejected(reason) })
	}
}

// react is PerformPromiseThen without a derived promise: it registers a reaction, or queues its job at once when the promise has settled.
// pending 时登记（settle 时统一 trigger）；已 settle 时也走 post
// 而非内联——保持"回调总在微任务时机执行"的 JS 保证。
func (promise *jsPromise[T]) react(fulfilled func(T), rejected func(error)) {
	switch promise.state {
	case jsStatePending:
		promise.reactions = append(promise.reactions, jsReaction[T]{fulfilled: fulfilled, rejected: rejected})
	case jsStateFulfilled:
		value := promise.value
		promise.executor.post(func() { fulfilled(value) })
	case jsStateRejected:
		reason := promise.reason
		promise.executor.post(func() { rejected(reason) })
	}
}

// then makes a native promise a thenable: the resolving functions become the reactions.
func (promise *jsPromise[T]) then(resolve func(jsOperand[T]), reject func(error)) error {
	promise.react(func(value T) { resolve(jsValue(value)) }, reject)
	return nil
}

// jsThen is promise.then(onFulfilled, onRejected). A nil handler passes the outcome through. A handler returns the derived promise's resolution, which may be a promise or thenable, or throws by returning an error.
func jsThen[T, U any](promise *jsPromise[T], onFulfilled func(T) (jsOperand[U], error), onRejected func(error) (jsOperand[U], error)) *jsPromise[U] {
	derived := newJSPromise[U](promise.executor)
	settle := func(operand jsOperand[U], err error) {
		if err != nil {
			derived.reject(err)
			return
		}
		derived.resolve(operand)
	}
	promise.react(func(value T) {
		if onFulfilled == nil {
			derived.resolve(jsValue(any(value).(U)))
			return
		}
		settle(onFulfilled(value))
	}, func(reason error) {
		if onRejected == nil {
			derived.reject(reason)
			return
		}
		settle(onRejected(reason))
	})
	return derived
}

// jsPromiseResolve is PromiseResolve(%Promise%, operand): a native promise is returned as is, any other value becomes a promise resolved with it, which for a thenable queues its adoption job.
func jsPromiseResolve[T any](executor *continuationExecutor, operand jsOperand[T]) *jsPromise[T] {
	if operand.promise != nil {
		return operand.promise
	}
	promise := newJSPromise[T](executor)
	promise.resolve(operand)
	return promise
}

// jsAwaiter is a place that can await: a coroutine that suspends the body, or a turn that suspends until a grant resumes it. register runs after the awaiter has stopped executing and must arrange for resume to be called from a reaction.
type jsAwaiter interface {
	jsExecutor() *continuationExecutor
	jsSuspend(register func(resume func()))
}

// jsAwait is Await(operand). The continuation runs in the reaction the settled promise queues. The error is the rejection reason.
func jsAwait[T any](awaiter jsAwaiter, operand jsOperand[T]) (T, error) {
	if turn, ok := awaiter.(*continuationTurn); ok {
		// The awaits the turn deferred come before the operand's promise is resolved and its reaction registered.
		// 先结算本 turn 推迟的 await，保证它们的反应先于
		// 本次 operand 的反应入队（顺序即微任务顺序）。
		turn.settleDeferred()
	}
	// Await 第一步永远是 PromiseResolve：原生 promise 原样，
	// 其他值包装（thenable 会额外排收养 job）。
	promise := jsPromiseResolve(awaiter.jsExecutor(), operand)
	var value T
	var err error
	// jsSuspend 分两步：register 把"恢复执行"挂到 promise 的反应
	// 上（在 awaiter 停手前完成），然后 turn/协程真正让出执行。
	awaiter.jsSuspend(func(resume func()) {
		promise.react(func(fulfilled T) {
			value = fulfilled
			resume()
		}, func(reason error) {
			err = reason
			resume()
		})
	})
	return value, err
}

func (turn *continuationTurn) jsExecutor() *continuationExecutor { return turn.executor }

// jsSuspend suspends the turn like awaitContinuation: the reaction registered by register grants the turn again.
func (turn *continuationTurn) jsSuspend(register func(resume func())) {
	register(turn.grant)
	turn.suspend()
}

// assertJSRunning rejects starting a coroutine from a goroutine that owns neither a turn nor a drain, where posting would run reactions inline.
func (executor *continuationExecutor) assertJSRunning() {
	executor.mu.Lock()
	running := executor.active != nil || executor.draining
	executor.mu.Unlock()
	if !running {
		panic("async function started outside its executor")
	}
}

type jsCoroutineStopped struct{}

// jsCoroutine runs straight-line Go code as JavaScript function body that suspends at await and yield. Each suspension hands a step back to the driver; the driver runs the step, and the coroutine resumes when a reaction calls advance. An abandoned suspended coroutine holds a goroutine until dispose.
type jsCoroutine struct {
	executor *continuationExecutor
	next     func() (func() bool, bool)
	stop     func()
	yield    func(func() bool) bool
	finish   func()
	finished bool
	stopped  bool
}

func newJSCoroutine(executor *continuationExecutor, body func(*jsCoroutine), finish func()) *jsCoroutine {
	co := &jsCoroutine{executor: executor, finish: finish}
	// 用 iter.Pull 把"直线 Go 代码"变成可挂起协程：body 跑在
	// pull 的 goroutine 上，每次挂起 yield 出一个 step 给驱动方；
	// 驱动方在合适的微任务时机跑 step 决定立即续跑还是等待反应。
	// jsCoroutineStopped 是 dispose 时的受控 panic：让被遗弃的
	// body 从挂起点正常退出（defer 照常跑）。
	co.next, co.stop = iter.Pull(func(yield func(func() bool) bool) {
		co.yield = yield
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(jsCoroutineStopped); !ok {
					panic(r)
				}
			}
		}()
		body(co)
	})
	return co
}

// advance resumes the body until its next suspension, running each suspension step. A step returns true to resume at once, as an async generator does at a yield with queued requests.
func (co *jsCoroutine) advance() {
	for {
		step, ok := co.next()
		if !ok {
			co.finished = true
			co.finish()
			return
		}
		if !step() {
			return
		}
	}
}

func (co *jsCoroutine) suspend(step func() bool) {
	if co.stopped || !co.yield(step) {
		co.stopped = true
		panic(jsCoroutineStopped{})
	}
}

func (co *jsCoroutine) jsExecutor() *continuationExecutor { return co.executor }

func (co *jsCoroutine) jsSuspend(register func(resume func())) {
	co.suspend(func() bool {
		register(co.advance)
		return false
	})
}

// dispose abandons a suspended body. Deferred calls run, but a deferred await does not resume.
func (co *jsCoroutine) dispose() {
	if co.finished {
		return
	}
	co.finished = true
	co.stopped = true
	co.stop()
}

// runJSAsync is calling an async function: body runs synchronously up to its first await, and the returned promise resolves with the body's return operand. Returning a promise or thenable resolves the async function's promise with it, which costs the adoption reactions of resolve(promise). A returned error rejects.
func runJSAsync[T any](executor *continuationExecutor, body func(*jsCoroutine) (jsOperand[T], error)) *jsPromise[T] {
	// AsyncFunctionStart：async 函数体同步执行到第一个 await 才挂起。
	executor.assertJSRunning()
	promise := newJSPromise[T](executor)
	var result jsOperand[T]
	var failure error
	// body 返回时经 finish 回调 settle 返回的 promise：返回 error
	// → reject；返回 promise/thenable → resolve 收养（再花收养
	// 所需的 reaction 数）。
	co := newJSCoroutine(executor, func(co *jsCoroutine) { result, failure = body(co) }, func() {
		if failure != nil {
			promise.reject(failure)
			return
		}
		promise.resolve(result)
	})
	co.advance()
	return promise
}
