//go:build windows

// 中文导读：测试环境设施——symlink_windows。
package testenv

import (
	"errors"

	"golang.org/x/sys/windows"
)

// symlinkPrivilegeMissing reports ERROR_PRIVILEGE_NOT_HELD, which Windows
// returns when SeCreateSymbolicLinkPrivilege is absent.
func symlinkPrivilegeMissing(err error) bool {
	return errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD)
}
