//go:build !linux

// 中文导读：socket 读取（非 linux）。
//
// 非 Linux 平台无原始读实现——直接用 net.Conn.Read。
package ai

import "net"

// newSocketReader has no raw read outside Linux; the connection's own Read serves every read.
func newSocketReader(net.Conn, string) func([]byte) (int, error) { return nil }
