//go:build !unix && !windows

// 中文导读：测试环境设施——unprivileged_other。
package testenv

import "testing"

// runUnprivileged continues in this process: outside Unix, no process identity bypasses the permission fixtures.
func runUnprivileged(*testing.T) bool { return false }
