//go:build windows

// 中文导读：chalk 样式级别（chalk 6.0.0 applyStyle）。
package codingagent

import "golang.org/x/sys/windows"

// windowsBuild is os.release() split into major, minor and build.
func windowsBuild() [3]int {
	major, minor, build := windows.RtlGetNtVersionNumbers()
	return [3]int{int(major), int(minor), int(build)}
}
