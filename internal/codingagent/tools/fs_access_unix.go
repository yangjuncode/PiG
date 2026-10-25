//go:build unix

// 中文导读：文件系统访问（unix 实现）。
package tools

import "syscall"

// accessReadWrite mirrors Node fs.access(path, R_OK | W_OK).
func accessReadWrite(path string) error {
	return syscall.Access(path, 0x4|0x2)
}
