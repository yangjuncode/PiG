//go:build windows

// 中文导读：子进程扩展宿主（D19）：扩展跑在独立进程，经 socket 帧协议与宿主通信；本目录是协议与宿主侧实现：socket_runtime_base_windows。
package subprocess

// secureSocketRuntimeBase accepts a Windows per-user base, the temp directory or %LOCALAPPDATA%\pig\s, as is.
func secureSocketRuntimeBase(string) error { return nil }
