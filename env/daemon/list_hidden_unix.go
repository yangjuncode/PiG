//go:build !windows

// 中文导读：env 守护进程——list_hidden_unix。
package daemon

// hiddenAttribute is whether the file has a hidden attribute, which only Windows files have.
func hiddenAttribute(string) bool { return false }
