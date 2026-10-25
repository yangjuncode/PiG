//go:build !windows

// 中文导读：路径规范化（paths.ts canonicalizePath）。
package codingagent

import "github.com/MichaelKinsy/PiG/internal/realpath"

func evalCanonicalPath(path string) (string, error) {
	return realpath.EvalSymlinks(path)
}
