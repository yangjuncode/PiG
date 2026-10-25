//go:build !linux && !windows && !darwin

// 中文导读：平台原生操作——clipboard_other。
package nativeplatform

func platformClipboard() *NativeClipboard { return nil }
