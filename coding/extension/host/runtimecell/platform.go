// 中文导读：运行时 cell：packed/fused 扩展运行单元（D20），对扩展观察上与 isolated 一致：platform。
package runtimecell

import "strings"

func packedRunnerName(goos, language string) string {
	switch strings.ToLower(language) {
	case "python":
		return "runner.py"
	case "go", "rust":
		if goos == "windows" {
			return "runner.exe"
		}
		return "runner"
	default:
		return "runner"
	}
}
