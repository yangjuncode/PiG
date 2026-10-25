//go:build !darwin && !linux && !windows

// 中文导读：Node errno 语义——posix_other。
package nodeerrno

import "syscall"

// platformPosixCodes names the errnos, among libuv's codes, that only some
// platforms define. This platform adds none.
var platformPosixCodes = map[syscall.Errno]string{}
