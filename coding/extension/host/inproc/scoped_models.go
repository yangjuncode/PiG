// 中文导读：把 Session 的 scoped-models 查询挂到 runner 的上下文动作上。
//
// 对应上游 agent-session.ts:_bindExtensionCore。只替换
// contextActions.GetScopedModels 这一个动作，不动 mode 绑定的其他
// 上下文；必须在事件派发前调用——派发时捕获的 dispatch context
// 持有当时的回调快照，绑定之后再捕获的 context 才能看到新值。
package inproc

import "github.com/MichaelKinsy/PiG/coding/extension"

// BindScopedModels binds the Session's scope without replacing the mode's other context actions. Bind before dispatch; existing contexts retain their captured callback.
// upstream: packages/coding-agent/src/core/agent-session.ts:_bindExtensionCore
func (r *Runner) BindScopedModels(getScopedModels func() []extension.ScopedModel) {
	r.contextActions.GetScopedModels = getScopedModels
}
