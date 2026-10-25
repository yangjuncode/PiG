//go:build !darwin && !windows

// 中文导读：平台原生操作——modifiers_other。
package nativeplatform

// isModifierPressed mirrors getNativePlatformHelper returning undefined on
// platforms other than macOS and Windows.
func isModifierPressed(string) bool { return false }
