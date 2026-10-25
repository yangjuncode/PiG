// 中文导读：EventStream——泛型异步事件流（utils/event-stream.ts 移植）。
//
// 上游 EventStream 的 Go 版：push/end 投递、排队缓冲、单消费者
// async iterator 消费、一次性 terminal result。AssistantMessageEventStream
// 是它的特化；ai/AGENTS.md 要求流保持顺序且只有一个终止结果。
package ai

// Ports packages/ai/src/utils/event-stream.ts:EventStream.

import (
	"context"
	"iter"
	"sync"
)

// EventStream delivers pushed events in order to its consumers and resolves one result. An event for which isComplete is true ends the stream and supplies the result through extractResult. Events pushed after the stream ended are dropped. Events are buffered without bound until read. Waiting consumers receive events in the order they began waiting.
type EventStream[T, R any] struct {
	isComplete    func(T) bool
	extractResult func(T) R

	mu      sync.Mutex
	queue   []T
	waiting []chan T
	done    bool
	result  R
	settled chan struct{}
	closed  bool
}

// NewEventStream creates a stream that ends when isComplete reports an event and then resolves with extractResult of that event.
func NewEventStream[T, R any](isComplete func(T) bool, extractResult func(T) R) *EventStream[T, R] {
	return &EventStream[T, R]{isComplete: isComplete, extractResult: extractResult, settled: make(chan struct{})}
}

func (s *EventStream[T, R]) settle(result R) {
	// 只允许一次 settle：closed 标记保证“只有一个终止结果”的契约，
	// 后到的事件不再改写 result。
	if s.closed {
		return
	}
	s.closed = true
	s.result = result
	//portlint:allow doubleclose settle returns early once closed is set, and every caller holds s.mu
	close(s.settled)
}

// Push delivers an event to the longest-waiting consumer, or buffers it. It ignores events after the stream ended.
func (s *EventStream[T, R]) Push(event T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return
	}
	// 完成事件触发 done 并把结果提取出来 settle——但完成事件本身
	// 仍然照常投递给消费者（上游语义：consumer 能看到 done 事件）。
	if s.isComplete(event) {
		s.done = true
		s.settle(s.extractResult(event))
	}
	// 有等待者时直接交给最早开始等待的消费者（保序且公平）；
	// 否则入缓冲队列等下一个 Events() 读者。
	if len(s.waiting) > 0 {
		waiter := s.waiting[0]
		s.waiting = s.waiting[1:]
		waiter <- event
		return
	}
	s.queue = append(s.queue, event)
}

// End ends the stream and releases every waiting consumer; buffered events remain readable. With a result it resolves Result unless Result is already resolved; without one Result stays unresolved, as upstream end() without an argument leaves it. Only the first result counts.
func (s *EventStream[T, R]) End(result ...R) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done = true
	// 带参数的 End 等价上游 end(result)；不带参数时 Result 保持
	// 未 resolve（对应上游 end() 无参的语义）。settle 内部幂等。
	if len(result) > 0 {
		s.settle(result[0])
	}
	s.finish()
}

func (s *EventStream[T, R]) finish() {
	s.done = true
	// 关闭所有等待通道（而非发送），让等待中的消费者以 ok=false
	// 退出循环；已缓冲的事件仍可被继续读走。
	for _, waiter := range s.waiting {
		close(waiter)
	}
	s.waiting = nil
}

// Events yields buffered and future events in order and returns once the stream ended and its buffer is empty.
func (s *EventStream[T, R]) Events() iter.Seq[T] {
	return func(yield func(T) bool) {
		for {
			s.mu.Lock()
			// 先排空缓冲：保证按 push 顺序交付。
			if len(s.queue) > 0 {
				event := s.queue[0]
				var zero T
				s.queue[0] = zero
				s.queue = s.queue[1:]
				s.mu.Unlock()
				if !yield(event) {
					return
				}
				continue
			}
			// 缓冲空且流已结束：终止迭代（含已读取的完成事件）。
			if s.done {
				s.mu.Unlock()
				return
			}
			// 否则注册等待通道（容量 1，Push 直接投递不会阻塞），
			// 解锁后挂起等下一个事件或 finish() 的 close。
			waiter := make(chan T, 1)
			s.waiting = append(s.waiting, waiter)
			s.mu.Unlock()
			event, ok := <-waiter
			if !ok || !yield(event) {
				return
			}
		}
	}
}

// Result blocks until the stream resolves its result.
func (s *EventStream[T, R]) Result() R {
	<-s.settled
	return s.result
}

// ResultContext is Result that gives up when ctx ends.
func (s *EventStream[T, R]) ResultContext(ctx context.Context) (R, error) {
	select {
	case <-s.settled:
		return s.result, nil
	case <-ctx.Done():
		var zero R
		return zero, context.Cause(ctx)
	}
}
