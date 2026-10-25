//go:build !windows

// 中文导读：测试环境设施——grant_unix；职责：InheritOthersRead is a no-op on POSIX: a new file's mode is set when it is。
package testenv

import (
	"os"
	"testing"
)

// InheritOthersRead is a no-op on POSIX: a new file's mode is set when it is
// created, so the directory cannot grant access to it beforehand.
func InheritOthersRead(testing.TB, string) {}

// GrantOthersRead lets principals other than the owner read the file at path
// (mode 0644), so an owner-only check must refuse it.
func GrantOthersRead(t testing.TB, path string) {
	t.Helper()
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
}
