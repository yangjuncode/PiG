//go:build pig_strip_node_extensions

// 中文导读：子进程扩展宿主（D19）：扩展跑在独立进程，经 socket 帧协议与宿主通信；本目录是协议与宿主侧实现：node_runtime_off。
package subprocess

import (
	"context"

	"github.com/MichaelKinsy/PiG/internal/pigstrip"
)

// pig additive (D92): this Piglet Binary compiled out the embedded Node runtime, so TypeScript and JavaScript extensions report the strip error instead of loading.
func init() { pigstrip.Strip(pigstrip.ListFeatures, pigstrip.NodeExtensions) }

func extractNodeRuntime(string) error { return errNodeExtensionsStripped() }

func nodeRuntimeUnavailable() error { return errNodeExtensionsStripped() }

func ensureNodeRuntime(context.Context) (string, error) { return "", errNodeExtensionsStripped() }
