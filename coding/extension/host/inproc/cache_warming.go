// 中文导读：cache-warming 决策事件的派发。
//
// EmitCacheWarmingDecision 对应上游 runner.ts 的 emitCacheWarmingDecision：
// 按扩展注册顺序逐个调用该事件的处理器，每个处理器可返回一个 action 覆盖
// 当前值，最后一个给出 action 的处理器获胜；没有任何处理器覆盖时回落到
// 事件自带的 Action。某个处理器报错只会被 recordHandlerError 记录，
// 不会中断后续处理器——这与上游“单个扩展失败不影响其他扩展”的契约一致。
//
// 实现要点：先把每个扩展的处理器切片做快照再迭代，避免派发期间
// 注册/注销改动遍历中的切片。
package inproc

import (
	"context"

	"github.com/MichaelKinsy/PiG/coding/extension"
)

// Ports packages/coding-agent/src/core/extensions/runner.ts

// EmitCacheWarmingDecision awaits handlers in registration order and returns the last supplied action, or the event's action when no handler overrides it. Handler errors are reported without stopping subsequent handlers.
func (r *Runner) EmitCacheWarmingDecision(ctx context.Context, event extension.CacheWarmingDecisionEvent) (extension.CacheWarmingAction, error) {
	if err := r.assertActive(); err != nil {
		return event.Action, err
	}
	dispatchCtx := r.dispatchContext(ctx)
	action := event.Action
	type handlers struct {
		path  string
		calls []extension.HandlerFn
	}
	snapshot := make([]handlers, 0, len(r.extensions))
	for _, ext := range r.extensions {
		snapshot = append(snapshot, handlers{ext.Path, append([]extension.HandlerFn(nil), ext.EventHandlers(event.Type)...)})
	}
	for _, ext := range snapshot {
		for _, handler := range ext.calls {
			result, err := callHandler(handler, event, dispatchCtx)
			if err != nil {
				r.recordHandlerError(ctx, ext.path, event.Type, err)
				continue
			}
			typed, ok := coerceResult[*extension.CacheWarmingDecisionEventResult](result)
			if ok && typed != nil && typed.Action != nil {
				action = *typed.Action
			}
		}
	}
	return action, nil
}
