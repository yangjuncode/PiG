//go:build !windows

// 中文导读：自更新层级选择（D39：SelfUpdateTier 变更前先选定 owner）。
package codingagent

import (
	"path/filepath"

	"golang.org/x/sys/unix"
)

// replacementDirectoryWritable checks the permissions required by the atomic
// rename. It intentionally does not open the running executable: Linux rejects
// that with ETXTBSY even though replacing it in a writable directory is valid.
func replacementDirectoryWritable(path string) bool {
	return directoryWritable(filepath.Dir(path))
}

func directoryWritable(dir string) bool {
	return unix.Access(dir, unix.W_OK|unix.X_OK) == nil
}
