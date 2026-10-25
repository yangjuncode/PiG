//go:build windows

// 中文导读：内部包——cwd_windows。
package nodepath

import "os"

// processCwd is process.cwd(); Windows has no $PWD indirection.
func processCwd() (string, error) { return os.Getwd() }
