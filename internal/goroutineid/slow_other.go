//go:build !(gc && (linux || darwin) && (amd64 || arm64))

// 中文导读：goroutine id 获取——slow_other。
package goroutineid

// current parses the stack header: this platform does not read the runtime's goroutine record.
func current() uint64 { return stackID() }
