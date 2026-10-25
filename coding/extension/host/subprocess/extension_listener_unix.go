//go:build !windows

// 中文导读：子进程扩展宿主（D19）：扩展跑在独立进程，经 socket 帧协议与宿主通信；本目录是协议与宿主侧实现：extension_listener_unix。
package subprocess

import "net"

// ListenExtension listens for one extension process on the AF_UNIX socket at
// sockPath and returns the address the process finds in PIG_EXT_SOCKET. The
// second argument, whether the process runs under Node, matters only on
// Windows.
func ListenExtension(sockPath string, _ bool) (net.Listener, string, error) {
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, "", err
	}
	return ln, sockPath, nil
}
