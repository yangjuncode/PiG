//go:build unix

// 中文导读：Pi 文件锁——eexist_unix。
package pilock

import (
	"errors"
	"syscall"
)

// isEEXIST reports Node's EEXIST code. fs.ErrExist also matches ENOTEMPTY, which Node reports as ENOTEMPTY.
func isEEXIST(err error) bool {
	return errors.Is(err, syscall.EEXIST)
}
