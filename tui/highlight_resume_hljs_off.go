//go:build pig_strip_syntax_highlight

// 中文导读：终端 UI 库（tui 包：渲染器、组件、输入解析）——highlight_resume_hljs_off。
package tui

// pig additive (D92): a Piglet Binary built with pig_strip_syntax_highlight links no highlight.js, so no code resumes.

func resetHighlightResumesLocked() {}

func highlightResumable(highlightEngine, string, string, bool, *Theme) ([]string, bool) {
	return nil, false
}
