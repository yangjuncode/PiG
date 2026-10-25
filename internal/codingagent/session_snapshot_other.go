//go:build !linux && !darwin && !windows

// 中文导读：会话快照（D112：条目记录留在盘上而非全内存）。
package codingagent

import "os"

// anonymousFile creates a file in dir and removes its name at once.
func anonymousFile(dir string) (*os.File, error) { return unlinkedTemp(dir) }

// cloneFile reports that this platform has no file clone: the snapshot is a copy.
func cloneFile(*os.File, string) (*os.File, bool) { return nil, false }
