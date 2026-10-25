// 中文导读：终端 UI 库（tui 包：渲染器、组件、输入解析）——js_upper。
package tui

import (
	"unicode/utf8"

	"github.com/MichaelKinsy/PiG/internal/jsstring"
)

// jsUpperFirstUnit is `s.charAt(0).toUpperCase() + s.slice(1)`: the first UTF-16 unit of s uppercased. A character beyond the BMP starts with a
// lone high surrogate, which toUpperCase leaves alone, so s is unchanged.
func jsUpperFirstUnit(s string) string {
	if s == "" {
		return s
	}
	r, size := utf8.DecodeRuneInString(s)
	if r > 0xffff {
		return s
	}
	rest := s[size:]
	return jsstring.UpperRune(r) + rest
}
