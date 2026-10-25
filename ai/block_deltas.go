// 中文导读：流式内容块的增量累积器。
//
// 消费者对一条流式消息按 `text += delta`、`parseStreamingJson(
// partialJson)` 逐事件应用增量（上游 packages/agent/src/proxy.ts
// 的 processProxyEvent）。BlockDeltas 为每个打开的内容块维护
// 状态：已累积字节放在一个增长的 buffer 里，工具调用参数从上
// 个 delta 结束处继续解析——让每个 delta 的成本与自身大小
// 成正比而不是与块总长成正比。块结束时调用 Release。
package ai

// BlockDeltas is the state a consumer of a streamed message keeps for its open blocks so that a delta costs the delta and not the block: the bytes of
// earlier deltas stay in one growing buffer, and the argument text of a tool call is parsed from where the previous delta ended. It is the machinery the
// provider stream builders use, for a consumer outside this package that applies `text += delta` and `parseStreamingJson(partialJson)` per event
// (packages/agent/src/proxy.ts processProxyEvent). The zero value is ready to use. A block ends with Release.
type BlockDeltas struct{ blocks streamedBlocks }

// AppendContent is current+delta for the text or thinking of the block at contentIndex.
func (deltas *BlockDeltas) AppendContent(contentIndex int, current, delta string) string {
	return deltas.blocks.appendContent(contentIndex, current, delta)
}

// AppendArguments is current+delta for the argument text of the tool call at contentIndex.
func (deltas *BlockDeltas) AppendArguments(contentIndex int, current, delta string) string {
	return deltas.blocks.appendArguments(contentIndex, current, delta)
}

// SetStreamingArguments is call.SetStreamingArguments(text) for the tool call at contentIndex, whose text is the previous call's text with the delta of
// AppendArguments added.
func (deltas *BlockDeltas) SetStreamingArguments(call *ToolCall, contentIndex int, text string) {
	call.setParsedArguments(deltas.blocks.streamingArguments(contentIndex, text))
}

// Release drops the buffers and parser of the block at contentIndex.
func (deltas *BlockDeltas) Release(contentIndex int) {
	deltas.blocks.release(contentIndex)
}
