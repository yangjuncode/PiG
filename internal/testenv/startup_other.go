//go:build !windows

// 中文导读：测试环境设施——startup_other。
package testenv

// ReportStartupIfRequested does nothing outside Windows, where a process has
// no STARTUPINFO to report.
func ReportStartupIfRequested() {}
