// 中文导读：内部包——realpath_windows；职责：EvalSymlinks is filepath.EvalSymlinks。
package realpath

import "path/filepath"

// EvalSymlinks is filepath.EvalSymlinks.
func EvalSymlinks(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}
