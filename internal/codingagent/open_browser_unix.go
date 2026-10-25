//go:build !windows

// 中文导读：internal/codingagent 的 open_browser_unix 实现。
package codingagent

import (
	"os/exec"
	"syscall"
)

// detachLauncher is Node's spawn option `detached: true`: libuv starts the child with setsid.
func detachLauncher(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
