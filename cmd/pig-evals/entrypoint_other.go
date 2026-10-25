//go:build !unix

// 中文导读：命令入口——entrypoint_other。
package main

import (
	"context"
	"errors"
)

func entrypoint(context.Context, []string) (int, error) {
	return 0, errors.New("the eval container entry point runs only on POSIX systems")
}
