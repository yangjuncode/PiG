//go:build !windows

// 中文导读：包管理器内部实现——capture_unix。
package packagemanager

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// Node child.kill() defaults to SIGTERM and leaves settlement to the close event.
func TerminatePackageCapture(process *os.Process) {
	_ = process.Signal(syscall.SIGTERM)
}

func PackageCaptureExitStatus(state *os.ProcessState) string {
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return "signal " + unix.SignalName(status.Signal())
	}
	return fmt.Sprintf("code %d", state.ExitCode())
}
