//go:build windows

// 中文导读：调试日志打开（平台实现）。
package codingagent

import "os"

// openDebugLog opens the debug log for appending. The temporary directory of a Windows user is private to that user.
func openDebugLog(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
}
