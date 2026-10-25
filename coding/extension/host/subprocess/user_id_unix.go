//go:build !windows

// 中文导读：子进程扩展宿主（D19）：扩展跑在独立进程，经 socket 帧协议与宿主通信；本目录是协议与宿主侧实现：user_id_unix。
package subprocess

import "os"

func currentUserID() int { return os.Getuid() }
