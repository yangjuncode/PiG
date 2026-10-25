//go:build pig_strip_mermaid

// 中文导读：internal/codingagent 的 mermaid_off 实现。
package codingagent

import (
	"github.com/MichaelKinsy/PiG/coding/extension"
	"github.com/MichaelKinsy/PiG/internal/pigstrip"
	"github.com/MichaelKinsy/PiG/tui"
)

// pig additive (D92): a Piglet Binary built with pig_strip_mermaid does not link internal/mermaid; mermaid code blocks stay raw, as in mermaid mode "off".

func init() { pigstrip.Strip(pigstrip.ListFeatures, pigstrip.Mermaid) }

func createMermaidMarkdownTransformer(func() string, *tui.Theme) extension.MarkdownTransformer {
	return rawMermaidMarkdown
}
