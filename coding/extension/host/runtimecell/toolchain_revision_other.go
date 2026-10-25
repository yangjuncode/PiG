//go:build !unix && !windows

// 中文导读：运行时 cell：packed/fused 扩展运行单元（D20），对扩展观察上与 isolated 一致：toolchain_revision_other。
package runtimecell

import (
	"fmt"
	"os"
)

func toolchainFileRevision(path string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	return fmt.Sprintf("%d:%d:%d", info.Size(), info.ModTime().UnixNano(), info.Mode()), true
}
