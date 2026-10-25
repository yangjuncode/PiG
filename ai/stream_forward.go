// 中文导读：lazy.ts forwardStream 的移植。
//
// forwardEvents = `for await (event of inner) outer.push(event)` +
// `outer.end(await inner.result())`：在 turn 内运行，source 的每个
// 事件经 push 转发；push 失败则返回该错误。注释详述该实现如何
// 让转发的每个事件花掉与上游相同的 reaction 数。
package ai

import "context"

// forwardEvents is lazy.ts forwardStream's `for await (const event of inner) outer.push(event)` followed by `outer.end(await inner.result())`, run inside turn: it pushes every event of source through push and returns source's result, or the first push failure.
//
// Every await of the loop resumes turn by one queued reaction. Here that reaction runs the loop body itself, the push and the next iterator step, at the queue position the resuming reaction holds, so a forwarded event costs the same reactions without handing execution to turn's goroutine and back. turn parks once and resumes at the reaction that ends the loop: the one that resolves the result await, or the one whose push failed.
func forwardEvents(turn *continuationTurn, ctx context.Context, source *AssistantMessageEventStream, push func(AssistantMessageEvent) error) (*AssistantMessage, error) {
	executor := turn.executor
	iterator := newContinuationEventIterator(ctx, source, executor)
	var failure error
	var result *continuationPromise[*AssistantMessage]
	var step func()
	react := func(delivery eventDelivery) {
		if !delivery.done {
			if failure = push(delivery.event); failure == nil {
				step()
				return
			}
			iterator.close()
			// The failed push ends the loop in this reaction; turn continues from it, as an await resumed here would.
			turn.grant()
			return
		}
		iterator.close()
		result = source.resultContinuation(executor)
		result.onResolvedGrant(turn)
	}
	step = func() { iterator.next().onResolved(react) }
	step()
	turn.park()
	if failure != nil {
		return nil, failure
	}
	// The result resolved before the reaction that resumed turn, and a resolved promise's value never changes.
	return result.value, nil
}
