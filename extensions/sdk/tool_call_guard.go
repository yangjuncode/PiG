// 中文导读：Go 扩展 SDK：扩展进程内使用的 API 实现——tool_call_guard；职责：IsToolCallEventType is Pi's isToolCallEventType (types.ts): it reports whether a tool_call event is a call of the named tool, built-in or custom. Pi's guard narro。
package sdk

// IsToolCallEventType is Pi's isToolCallEventType (types.ts): it reports whether a tool_call event is a call of the named tool, built-in or custom. Pi's guard narrows the
// event's TypeScript type and compares event.toolName with toolName; here the event is the handler's map, and an event without a string toolName is no call of any tool.
func IsToolCallEventType(toolName string, event map[string]any) bool {
	name, ok := event["toolName"].(string)
	return ok && name == toolName
}
