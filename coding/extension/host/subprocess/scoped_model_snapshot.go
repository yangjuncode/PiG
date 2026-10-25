// 中文导读：子进程扩展宿主（D19）：扩展跑在独立进程，经 socket 帧协议与宿主通信；本目录是协议与宿主侧实现：scoped_model_snapshot。
package subprocess

import "github.com/MichaelKinsy/PiG/coding/extension"

// pig additive (D19): only the subprocess boundary projects native model pointers into JSON metadata; the Session and native Context retain the original scope identities.
type scopedModelSnapshot struct {
	Model         extension.ModelValue `json:"model"`
	ThinkingLevel string               `json:"thinkingLevel,omitempty"`
}

func snapshotScopedModels(models []extension.ScopedModel) []scopedModelSnapshot {
	result := make([]scopedModelSnapshot, len(models))
	for i, scoped := range models {
		result[i] = scopedModelSnapshot{Model: extension.ModelValue{Model: scoped.Model}, ThinkingLevel: string(scoped.ThinkingLevel)}
	}
	return result
}
