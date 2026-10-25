//go:build !linux

// 中文导读：测试环境设施——hardlinks_other。
package testenv

import "testing"

// RunWithHardLinksRefused skips: only Linux refuses hard links through the kernel's seccomp filter, and only Linux and Android fall back when a link is refused.
func RunWithHardLinksRefused(t *testing.T) bool {
	t.Helper()
	t.Skip("hard-link refusal is simulated only on Linux")
	return false
}
