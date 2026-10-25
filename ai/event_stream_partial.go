// 中文导读：流事件的 partial 消息变换。
//
// mapAssistantEventPartial 对事件携带的 partial assistant message
// 应用变换：映射结果与原 partial 相同则原样返回事件（不复制），
// 否则返回替换 partial 后的新事件——供快照/观察层按事件类型
// 逐分支处理。
package ai

// mapAssistantEventPartial returns event with its partial replaced by mapMessage's result. An event whose partial maps to itself is returned as it is, without a copy.
func mapAssistantEventPartial(event AssistantMessageEvent, mapMessage func(*AssistantMessage) *AssistantMessage) AssistantMessageEvent {
	switch typed := event.(type) {
	case StartEvent:
		if partial := mapMessage(typed.Partial); partial != typed.Partial {
			typed.Partial = partial
			return typed
		}
	case TextStartEvent:
		if partial := mapMessage(typed.Partial); partial != typed.Partial {
			typed.Partial = partial
			return typed
		}
	case TextDeltaEvent:
		if partial := mapMessage(typed.Partial); partial != typed.Partial {
			typed.Partial = partial
			return typed
		}
	case TextEndEvent:
		if partial := mapMessage(typed.Partial); partial != typed.Partial {
			typed.Partial = partial
			return typed
		}
	case ThinkingStartEvent:
		if partial := mapMessage(typed.Partial); partial != typed.Partial {
			typed.Partial = partial
			return typed
		}
	case ThinkingDeltaEvent:
		if partial := mapMessage(typed.Partial); partial != typed.Partial {
			typed.Partial = partial
			return typed
		}
	case ThinkingEndEvent:
		if partial := mapMessage(typed.Partial); partial != typed.Partial {
			typed.Partial = partial
			return typed
		}
	case ToolCallStartEvent:
		if partial := mapMessage(typed.Partial); partial != typed.Partial {
			typed.Partial = partial
			return typed
		}
	case ToolCallDeltaEvent:
		if partial := mapMessage(typed.Partial); partial != typed.Partial {
			typed.Partial = partial
			return typed
		}
	case ToolCallEndEvent:
		if partial := mapMessage(typed.Partial); partial != typed.Partial {
			typed.Partial = partial
			return typed
		}
	}
	return event
}
