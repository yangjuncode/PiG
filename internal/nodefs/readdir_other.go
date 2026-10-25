//go:build !windows

// 中文导读：Node fs 语义——readdir_other。
package nodefs

import "os"

// readDir sorts the entries by name, as libuv's Unix scandir does with strcmp.
func readDir(name string) ([]os.DirEntry, error) {
	return os.ReadDir(name)
}
