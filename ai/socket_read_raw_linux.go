//go:build linux && !race

// 中文导读：非 race 构建的 Linux 原始 socket 读。
//
// 直接 syscall 读 fd，去掉中间层（!race 构建）。
package ai

import (
	"syscall"
	"unsafe"
)

// readSocketOnce issues one read(2) of fd into buffer as a raw system call, which neither enters nor leaves a blocking system call.
func readSocketOnce(fd uintptr, buffer []byte) (uintptr, syscall.Errno) {
	//nolint:gosec // G103: read(2) into the caller's non-empty buffer; RawSyscall keeps the pointer alive for the call (go:uintptrkeepalive).
	n, _, errno := syscall.RawSyscall(syscall.SYS_READ, fd, uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)))
	return n, errno
}
