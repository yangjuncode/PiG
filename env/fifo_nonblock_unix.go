//go:build unix

// 中文导读：env 环境子系统——fifo_nonblock_unix。
package env

import "syscall"

// nonBlock opens a FIFO without waiting for its other end (O_NONBLOCK).
const nonBlock = syscall.O_NONBLOCK
