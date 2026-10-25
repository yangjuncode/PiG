//go:build !unix

// 中文导读：env 环境子系统——fifo_nonblock_other。
package env

// nonBlock is zero where there are no FIFOs to open.
const nonBlock = 0
