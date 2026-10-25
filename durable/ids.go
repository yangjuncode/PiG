// 中文导读：durable 子系统：可持久化/可恢复的代理运行——ids；对应上游 packages/durable/src/ids.ts。
package durable

// Ports packages/durable/src/ids.ts

// IdFromNumber applies an ID kind at a trusted numeric allocation or decoding boundary.
func IdFromNumber[I Id](value int64) I {
	return I(value)
}

// SeqFromNumber applies the commit-sequence kind at a trusted storage boundary.
func SeqFromNumber(value int64) Seq {
	return Seq(value)
}
