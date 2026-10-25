// 中文导读：AbortSignal 语义的两个原语。
//
// OperationSignal（upstream abort.ts:operationSignal）：把可选信号
// 归一化——nil 换成永不取消的 context。RaceWithAbortSignal：让
// 操作与信号赛跑——信号先取消则返回取消因，操作本身不中断、
// 在自有 goroutine 跑完并丢弃结果（对应上游 Promise.race 中
// 被抛弃分支仍 settle 的语义）；已取消的信号立即获胜。
package ai

import "context"

// OperationSignal normalizes an optional public signal without imposing a deadline: a nil signal becomes a context that is never cancelled.
//
// upstream: packages/ai/src/utils/abort.ts:operationSignal
func OperationSignal(signal context.Context) context.Context {
	if signal == nil {
		//portlint:allow ctxmapping a nil signal is the case this function normalizes: abort.ts operationSignal substitutes a signal that never aborts
		return context.Background()
	}
	return signal
}

// RaceWithAbortSignal runs operation and stops waiting for it when signal is cancelled. The operation is never interrupted: it runs to completion on its own goroutine and its result is dropped, so an abandoned operation is observed through settlement as in the upstream promise race. The cancellation cause is returned when the signal wins, including a signal that is already cancelled when the race starts; otherwise the operation's value and error are returned. A nil signal waits for the operation alone.
//
// upstream: packages/ai/src/utils/abort.ts:raceWithAbortSignal
func RaceWithAbortSignal[T any](signal context.Context, operation func() (T, error)) (T, error) {
	var zero T
	if signal == nil {
		return operation()
	}
	type outcome struct {
		value T
		err   error
	}
	settled := make(chan outcome, 1)
	go func() {
		value, err := operation()
		settled <- outcome{value, err}
	}()
	if signal.Err() != nil {
		return zero, context.Cause(signal)
	}
	select {
	case result := <-settled:
		return result.value, result.err
	case <-signal.Done():
		return zero, context.Cause(signal)
	}
}
