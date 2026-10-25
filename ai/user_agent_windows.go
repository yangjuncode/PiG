//go:build windows

// 中文导读：User-Agent 平台段（windows）。
package ai

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// piUserAgentRelease returns the Windows version, matching what Node's
// os.release() reports on Windows (major.minor.build).
func piUserAgentRelease() string {
	info := windows.RtlGetVersion()
	return fmt.Sprintf("%d.%d.%d", info.MajorVersion, info.MinorVersion, info.BuildNumber)
}
