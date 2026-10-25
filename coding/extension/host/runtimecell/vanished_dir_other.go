//go:build !windows

// 中文导读：运行时 cell：packed/fused 扩展运行单元（D20），对扩展观察上与 isolated 一致：vanished_dir_other。
package runtimecell

// windowsDeletion reports nothing: only Windows keeps a directory visible, and its open failing, while another process deletes it.
func windowsDeletion(error) (pending, denied bool) { return false, false }
