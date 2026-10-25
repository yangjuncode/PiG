// 中文导读：终端 UI 库（tui 包：渲染器、组件、输入解析）——tui_mode；职责：TuiMode is the renderer mode. Mirrors upstream TuiMode (tui.ts:448)。
package tui

// TuiMode is the renderer mode. Mirrors upstream TuiMode (tui.ts:448).
type TuiMode string

const (
	TuiModeRegular    TuiMode = "regular"
	TuiModeFullscreen TuiMode = "fullscreen"
)
