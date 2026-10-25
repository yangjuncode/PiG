//go:build !unix

// 中文导读：aot piglet 的 AOT 运行时扩展：lock_other。
package aotruntime

import (
	"errors"
	"os/exec"
)

type fileLock struct{}

func lockFile(string, bool) (*fileLock, bool, error) {
	return nil, false, errors.New("the aot runtime builds only on linux and darwin")
}

func (*fileLock) Unlock() {}

// ownProcessGroup bounds the wait for output pipes; without process groups nothing else is set.
func ownProcessGroup(cmd *exec.Cmd) { cmd.WaitDelay = processWaitDelay }

func endProcessGroup(*exec.Cmd) {}
