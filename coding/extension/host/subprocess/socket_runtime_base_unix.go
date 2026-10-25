//go:build !windows

// 中文导读：子进程扩展宿主（D19）：扩展跑在独立进程，经 socket 帧协议与宿主通信；本目录是协议与宿主侧实现：socket_runtime_base_unix。
package subprocess

import (
	"fmt"
	"os"
	"syscall"
)

// secureSocketRuntimeBase accepts dir as the parent of a Host's private runtime directory only when it is a real
// directory owned by the current user, and makes it private (0700). The base can sit in a shared directory such as
// /tmp, where another user who could write the base could rename the Host's runtime directory and substitute one
// holding their own sockets before the Host binds.
func secureSocketRuntimeBase(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("extension socket directory %s is not a directory", dir)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != currentUserID() {
		return fmt.Errorf("extension socket directory %s is owned by uid %d, not %d", dir, stat.Uid, currentUserID())
	}
	if info.Mode().Perm()&0o077 != 0 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}
