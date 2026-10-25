//go:build !windows

// 中文导读：Windows 自更新（getPackageDir）。
package codingagent

// loadedSharedObjects is used only by the Windows npm self-update, which
// PrepareWindowsNpmSelfUpdate skips on every other platform.
func loadedSharedObjects() []string { return nil }

// longWindowsPath expands 8.3 short names, which exist only on Windows.
func longWindowsPath(path string) string { return path }
