// 中文导读：终端 UI 库（tui 包：渲染器、组件、输入解析）——native_modifiers；职责：ModifierKey mirrors upstream native-platform.ts ModifierKey。
package tui

import "github.com/MichaelKinsy/PiG/internal/nativeplatform"

// ModifierKey mirrors upstream native-platform.ts ModifierKey.
type ModifierKey string

const (
	ModifierShift   ModifierKey = "shift"
	ModifierCommand ModifierKey = "command"
	ModifierControl ModifierKey = "control"
	ModifierOption  ModifierKey = "option"
)

// IsNativeModifierPressed mirrors upstream native-modifiers.ts
// isNativeModifierPressed: it asks the operating system whether a modifier key
// is held right now, and reports false when no helper is available.
func IsNativeModifierPressed(key ModifierKey) bool {
	return nativeplatform.IsModifierPressed(string(key))
}
