//go:build !linux

// 中文导读：平台原生操作——clipboard_shutdown_other。
package nativeplatform

// ShutdownClipboard has no private worker to join on platforms whose caller owns the complete native operation.
func ShutdownClipboard() {}
