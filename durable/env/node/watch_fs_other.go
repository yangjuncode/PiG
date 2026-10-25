//go:build !linux

// 中文导读：durable Node 环境——watch_fs_other。
package node

// filesystemType is unavailable off Linux, where no file system is treated as unreliable.
func filesystemType(string) (uint32, bool) { return 0, false }
