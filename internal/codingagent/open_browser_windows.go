//go:build windows

// 中文导读：internal/codingagent 的 open_browser_windows 实现。
package codingagent

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// detachLauncher is Node's spawn option `detached: true`: libuv adds DETACHED_PROCESS and CREATE_NEW_PROCESS_GROUP.
func detachLauncher(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS}
}
