// 中文导读：流式文本的零拷贝累积字符串。
//
// accumulatedString 让“current + delta”式追加不每次复制前文：
// 返回的字符串共享一个只增的 buffer，已返回的前缀不会被改写
// （保持不可变）。当传入的 current 与 buffer 不符（持有者换掉
// 了累积串）时从 current 重建 buffer。
package ai

import "strings"

// accumulatedString appends deltas to a growing string without copying the earlier bytes each time. The strings it returns share its buffer; the buffer only ever grows past the bytes already returned, so they stay immutable.
type accumulatedString struct{ buffer strings.Builder }

// append is current+delta. When current is not what the buffer holds, because the owner replaced it, the buffer restarts from current.
func (accumulated *accumulatedString) append(current, delta string) string {
	if accumulated.buffer.String() != current {
		accumulated.buffer.Reset()
		accumulated.buffer.Grow(len(current) + len(delta))
		accumulated.buffer.WriteString(current)
	}
	accumulated.buffer.WriteString(delta)
	return accumulated.buffer.String()
}
