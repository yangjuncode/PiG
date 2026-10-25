//go:build !windows

// 中文导读：内部包——transient_other。
package fsretry

// TransientRename reports whether a rename failure is a transient lock worth
// retrying. Unix renames are not subject to the Windows sharing model, so
// EACCES and EPERM there are real permission failures.
func TransientRename(error) bool { return false }
