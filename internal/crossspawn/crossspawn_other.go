//go:build !windows

// 中文导读：跨平台 spawn——crossspawn_other。
package crossspawn

import (
	"context"
	"os/exec"
)

func command(ctx context.Context, _, name string, args []string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}
