//go:build !windows

// 中文导读：外部编辑器（openExternalEditor）。
package codingagent

import (
	"context"
	"os/exec"

	"github.com/MichaelKinsy/PiG/internal/linkerexec"
)

// editorCommand runs the editor directly, as upstream spawns it without a
// shell outside Windows.
func editorCommand(ctx context.Context, name string, args []string) *exec.Cmd {
	return linkerexec.CommandContext(ctx, name, args...)
}
