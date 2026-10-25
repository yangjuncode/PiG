// 中文导读：Runner 的只读访问器。
//
// GetModelRegistry 对应上游 runner.getModelRegistry（runner.ts:843-845）：
// 返回 runner 持有的模型注册表，可能是 BindCore/BindTools 绑定时被
// 替换过的那份，未绑定前为 nil。
//
// GetActiveTools 对应上游 runner.getActiveTools（runner.ts:868-871）：
// 通过绑定的 runtime action 读取“已声明给模型的工具”名单。两个前置
// 检查分别对应上游行为：runner 已失效时返回 stale 错误；runtime
// 尚未绑定（action 为 nil）时返回 ErrRuntimeNotInitialized，
// 对应上游 notInitialized 抛错的路径。
package inproc

import "github.com/MichaelKinsy/PiG/coding/extension"

// GetModelRegistry is upstream runner.getModelRegistry (runner.ts:843-845): the registry the runner was given, or the one a mode's BindCore or BindTools replaced it with. It is nil before any binding supplies one.
func (r *Runner) GetModelRegistry() extension.ModelRegistry {
	return r.contextActions.ModelRegistry
}

// GetActiveTools is upstream runner.getActiveTools (runner.ts:868-871): the names of the tools declared to the model, read through the runtime's bound action. A stale runner returns its stale error, and a runtime no host has bound returns [extension.ErrRuntimeNotInitialized], as upstream's `notInitialized` throws.
func (r *Runner) GetActiveTools() ([]string, error) {
	if err := r.assertActive(); err != nil {
		return nil, err
	}
	if r.runtime.GetActiveTools == nil {
		return nil, extension.ErrRuntimeNotInitialized
	}
	return r.runtime.GetActiveTools(), nil
}
