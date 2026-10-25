//go:build !windows

// 中文导读：测试环境设施——directory_link_other。
package testenv

import "os"

// createDirectoryLink creates newname as a symbolic link to the directory oldname.
func createDirectoryLink(oldname, newname string) error {
	return os.Symlink(oldname, newname)
}
