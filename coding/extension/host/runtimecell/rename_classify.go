// 中文导读：运行时 cell：packed/fused 扩展运行单元（D20），对扩展观察上与 isolated 一致：rename_classify。
package runtimecell

import "github.com/MichaelKinsy/PiG/internal/fsretry"

// renameRetryable reports whether a rename failed on a transient Windows lock.
func renameRetryable(err error) bool { return fsretry.TransientRename(err) }
