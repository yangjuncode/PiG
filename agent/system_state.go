package agent

import (
	"encoding/json"
	"slices"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
)

// toolDeclarations 把当前工具表转成写进系统消息的工具声明列表——模型
// 能调用什么工具完全由它决定，SetTools 后下一轮请求即生效。
func toolDeclarations(current []AgentTool) []ai.ToolSchema {
	tools := make([]ai.ToolSchema, len(current))
	for i, t := range current {
		tools[i] = ai.ToToolDeclaration(t.Schema())
	}
	return tools
}

// toolSchemas are the loadout's schemas as the tools return them. Comparing them needs no declaration copy, because
// ai.DeclarationsEqual ignores prompt guidelines and ai.GetToolStateChanges copies only the declarations it adds.
func toolSchemas(current []AgentTool) []ai.ToolSchema {
	schemas := make([]ai.ToolSchema, len(current))
	for i, t := range current {
		schemas[i] = t.Schema()
	}
	return schemas
}

func systemMessages(messages []AgentMessage) []ai.Message {
	var systems []ai.Message
	for _, m := range messages {
		if m.System != nil {
			systems = append(systems, *m.System)
		}
	}
	return systems
}

// declareToolChanges replaces pending tool intent with the delta from the committed transcript to the executable loadout.
//
// 工具增删（扩展注册/卸载工具）发生在两次请求之间时，需要告诉模型
// "工具集变了"：对比转录里最近一条 system 消息记录的工具集与当前实际
// 工具表，把差集写进一条带 ToolsAdded/ToolsRemoved 的 system 消息，
// 插在下一批待发消息中。没变时原样返回——不制造多余消息。
// 先在待发批次里找最后一条 system 消息（它就是本次要更新的
// "工具视图基线"载体）。
func (h *loopHost) declareToolChanges(context, pending []AgentMessage) []AgentMessage {
	return declareToolChanges(h.tools(), context, pending)
}

func declareToolChanges(tools []AgentTool, context, pending []AgentMessage) []AgentMessage {
	index := -1
	for i, pendingMessage := range slices.Backward(pending) {
		if pendingMessage.System != nil {
			index = i
			break
		}
	}
	if len(pending) == 0 {
		changes := ai.GetToolStateChangesFrom(systemOnly(context), toolSchemas(tools))
		if len(changes.ToolsAdded) == 0 && len(changes.ToolsRemoved) == 0 {
			return pending
		}
		return []AgentMessage{withToolChanges(AgentMessage{System: &ai.SystemMessage{Content: ai.SystemText(""), Timestamp: time.Now().UnixMilli()}}, changes)}
	}
	baseline := slices.Clone(pending)
	if index >= 0 {
		// 先清掉这条 system 上旧的增删记录再算差分——差分是相对
		// "转录已声明的工具集"算的，旧的声明不能参与本次比较。
		baseline[index] = withToolChanges(pending[index], ai.ToolStateChanges{})
	}
	// 算差分：转录+待发消息里已声明的工具集 → 当前实际工具表。
	changes := ai.GetToolStateChangesFrom(slices.Concat(systemOnly(context), systemOnly(baseline)), toolSchemas(tools))
	unchanged := len(changes.ToolsAdded) == 0 && len(changes.ToolsRemoved) == 0
	if index >= 0 {
		old := pending[index].System
		if unchanged && len(old.ToolsAdded) == 0 && len(old.ToolsRemoved) == 0 {
			return pending
		}
		baseline[index] = withToolChanges(pending[index], changes)
		return baseline
	}
	if unchanged {
		return pending
	}
	// 待发批次里没有 system 消息可挂：造一条空指令的 system 消息，
	// 插在第一个非 system 消息之前，专门携带工具差分。
	update := withToolChanges(AgentMessage{System: &ai.SystemMessage{Content: ai.SystemText(""), Timestamp: time.Now().UnixMilli()}}, changes)
	index = slices.IndexFunc(pending, func(m AgentMessage) bool { return m.System == nil })
	if index < 0 {
		index = len(pending)
	}
	return slices.Concat(pending[:index], []AgentMessage{update}, pending[index:])
}

// systemOnly is messages without the ones that are not system messages. The replay of tool declarations reads only system messages, so a replay of the result equals a replay of messages; it walks a history of any length with a pointer test per message, and the replay itself walks the few system messages.
func systemOnly(messages []AgentMessage) []AgentMessage {
	count := 0
	for i := range messages {
		if messages[i].System != nil {
			count++
		}
	}
	if count == len(messages) {
		return messages
	}
	out := make([]AgentMessage, 0, count)
	for i := range messages {
		if messages[i].System != nil {
			out = append(out, messages[i])
		}
	}
	return out
}

func withToolChanges(message AgentMessage, changes ai.ToolStateChanges) AgentMessage {
	value := *message.System
	value.ToolsAdded = changes.ToolsAdded
	value.ToolsRemoved = changes.ToolsRemoved
	return AgentMessage{System: &value}
}

// systemMessageWire is the object a system message decodes from.
type systemMessageWire struct {
	Content      json.RawMessage    `json:"content"`
	Sections     ai.OrderedSections `json:"sections"`
	ToolsAdded   []ai.ToolSchema    `json:"toolsAdded"`
	ToolsRemoved []ai.ToolReference `json:"toolsRemoved"`
	Timestamp    jsInt64            `json:"timestamp"`
}

func decodeSystemMessage(data []byte) (*ai.SystemMessage, error) {
	var wire systemMessageWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, err
	}
	content, err := decodeSystemContent(wire.Content)
	if err != nil {
		return nil, err
	}
	return &ai.SystemMessage{Content: content, Sections: wire.Sections, ToolsAdded: wire.ToolsAdded, ToolsRemoved: wire.ToolsRemoved, Timestamp: int64(wire.Timestamp)}, nil
}

// decodeSystemContent decodes a system message's content: a string or text blocks, and nil when it is null or missing (Pi parses a session line without validation; the context projects { ...message, content: "" }, session-manager.ts sessionEntryToContextMessages).
func decodeSystemContent(raw []byte) (ai.SystemContent, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return ai.SystemText(text), nil
	}
	var blocks ai.SystemTextBlocks
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}
	return blocks, nil
}

// projectSystemPrompt preserves the active tool declarations while replacing instructions at the provider boundary.
//
// SetSystemPrompt 的实现机制：不改写历史转录，只在发往 provider 的消息
// 边界上把系统消息的指令部分替换成投影值（工具声明等成员保留原样）。
// 这样 ClearSystemPrompt 后历史仍自洽，重放不会看到两套提示打架。
func projectSystemPrompt(messages []ai.Message, prompt string) []ai.Message {
	head := ai.GetCurrentSystemMessage(messages)
	if head == nil {
		head = &ai.SystemMessage{}
	}
	head.Content = ai.SystemText(prompt)
	head.Sections = nil
	projected := []ai.Message{*head}
	for _, m := range messages {
		if _, ok := m.(ai.SystemMessage); !ok {
			projected = append(projected, m)
		}
	}
	return projected
}
