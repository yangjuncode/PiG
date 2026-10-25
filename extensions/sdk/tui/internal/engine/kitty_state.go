// 中文导读：Go 扩展 SDK 的 TUI 引擎——kitty_state；职责：SetKittyProtocolActive mirrors upstream keys.ts global Kitty state。
package engine

import "sync/atomic"

var kittyProtocolActive atomic.Bool

// SetKittyProtocolActive mirrors upstream keys.ts global Kitty state.
func SetKittyProtocolActive(active bool) { kittyProtocolActive.Store(active) }

// IsKittyProtocolActive reports the global Kitty keyboard protocol state.
func IsKittyProtocolActive() bool { return kittyProtocolActive.Load() }
