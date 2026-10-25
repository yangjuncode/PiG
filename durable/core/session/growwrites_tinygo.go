//go:build tinygo

// 中文导读：durable 核心会话——growwrites_tinygo。
package session

// growWrites leaves writes to grow by append on the TinyGo core: TinyGo's slices.Grow allocates a zeroed temporary of the
// added length as well (mirror.go), and its instantiation is an allocation site of its own (budget/allocs.json).
func growWrites(writes []Write, n int) []Write { return writes }
