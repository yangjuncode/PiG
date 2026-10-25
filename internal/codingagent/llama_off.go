//go:build pig_strip_llama_cpp

// 中文导读：internal/codingagent 的 llama_off 实现。
package codingagent

import (
	"strings"

	"github.com/MichaelKinsy/PiG/internal/pigstrip"
)

// pig additive (D92): this build has no built-in llama.cpp extension. Nothing registers the llama.cpp provider or /llama, and
// `builtin:llama.cpp` is skipped like a stripped built-in.
func init() {
	pigstrip.Strip(pigstrip.ListExtensions, strings.TrimPrefix(LlamaExtensionPath, BuiltinPathPrefix))
}
