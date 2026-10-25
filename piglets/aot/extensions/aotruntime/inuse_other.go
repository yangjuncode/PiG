//go:build !linux

// 中文导读：aot piglet 的 AOT 运行时扩展：inuse_other。
package aotruntime

// inUse reports whether a running process executes a file inside dir. Without /proc the store cannot tell, so it relies on the recency of use.
func inUse(string) bool { return false }
