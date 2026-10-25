// 中文导读：交互模式 TUI 实现。
package codingagent

import "github.com/MichaelKinsy/PiG/coding/extension"

// extensionScopedModels reads the Session's already-resolved scope; SDK queries do not resolve catalogs or credentials.
func (m *InteractiveMode) extensionScopedModels() []extension.ScopedModel {
	if m.opts.SessionHandle != nil {
		return m.opts.SessionHandle.ScopedModels()
	}
	return []extension.ScopedModel{}
}
