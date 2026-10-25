//go:build windows

// 中文导读：CLI 层：RPC 关闭流程。
package cli

import "os"

// statusControlCExit is STATUS_CONTROL_C_EXIT (0xC000013A), the exit status
// Windows gives a console process that its default control handler ends.
const statusControlCExit uint32 = 0xC000013A

// dieBySignal ends the process as Windows' default console control handler
// does for Node without a listener: at once, with STATUS_CONTROL_C_EXIT.
func dieBySignal(os.Signal) {
	status := statusControlCExit
	os.Exit(int(int32(status)))
}
