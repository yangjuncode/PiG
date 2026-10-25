//go:build !windows

// 中文导读：测试环境设施——symlink_other。
package testenv

// symlinkPrivilegeMissing reports false: creating a symbolic link needs no
// special privilege outside Windows.
func symlinkPrivilegeMissing(error) bool { return false }
