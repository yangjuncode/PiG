//go:build windows

// 中文导读：包管理器内部实现——capture_windows；职责：Windows implements Node child.kill() as process termination。
package packagemanager

import (
	"fmt"
	"os"
)

// Windows implements Node child.kill() as process termination.
func TerminatePackageCapture(process *os.Process) {
	_ = process.Kill()
}

func PackageCaptureExitStatus(state *os.ProcessState) string {
	return fmt.Sprintf("code %d", state.ExitCode())
}
