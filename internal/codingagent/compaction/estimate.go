package compaction

import (
	"encoding/json"
	"slices"
	"unicode/utf8"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/internal/codingagent"
	"github.com/MichaelKinsy/PiG/internal/jsstring"
)

// estimatedImageChars is Pi's per-image character estimate (ESTIMATED_IMAGE_CHARS).
const estimatedImageChars = 4800

// jsLength returns JavaScript's String.length for s: its UTF-16 code units. Runs of ASCII are counted eight bytes at a time.
func jsLength(s string) int {
	units := 0
	for len(s) > 0 {
		if s[0] < utf8.RuneSelf {
			n := jsstring.ASCIIPrefix(s)
			s = s[n:]
			units += n
			continue
		}
		r, size := utf8.DecodeRuneInString(s)
		s = s[size:]
		if r > 0xFFFF {
			units += 2
		} else {
			units++
		}
	}
	return units
}

func ceilQuarter(chars int) int { return (chars + 3) / 4 }

// contentBlocksChars sums text lengths and a fixed estimate per image
// (compaction.ts estimateTextAndImageContentChars).
func contentBlocksChars[T any](blocks []T) int {
	chars := 0
	for _, block := range blocks {
		switch block := any(block).(type) {
		case ai.TextContent:
			chars += jsLength(block.Text)
		case ai.ImageContent:
			chars += estimatedImageChars
		}
	}
	return chars
}

// customContentChars sizes a decoded custom-message content value: a string or
// an array of content-block objects.
func customContentChars(content any) int {
	switch content := content.(type) {
	case string:
		return jsLength(content)
	case []any:
		chars := 0
		for _, block := range content {
			fields, _ := block.(map[string]any)
			switch fields["type"] {
			case "text":
				text, _ := fields["text"].(string)
				chars += jsLength(text)
			case "image":
				chars += estimatedImageChars
			}
		}
		return chars
	}
	return 0
}

// estimateSystemTokens sizes a system message with its sections and added
// tool declarations.
func estimateSystemTokens(system ai.SystemMessage) int {
	chars := 0
	switch content := system.Content.(type) {
	case ai.SystemText:
		chars += jsLength(string(content))
	case ai.SystemTextBlocks:
		chars += contentBlocksChars(content)
	}
	for _, section := range system.Sections {
		if section.Value != nil {
			chars += jsLength(*section.Value)
		}
	}
	if len(system.ToolsAdded) > 0 {
		chars += ai.JSONStringifyLength(system.ToolsAdded)
	}
	return ceilQuarter(chars)
}

// EstimateTokens estimates one context message with the chars/4 heuristic,
// counting JavaScript string lengths (compaction.ts estimateTokens). Unknown
// roles count as zero.
func EstimateTokens(message agent.AgentMessage) int {
	switch {
	case message.System != nil:
		return estimateSystemTokens(*message.System)
	case message.User != nil:
		return ceilQuarter(contentBlocksChars(message.ContentBlocks()))
	case message.Assistant != nil:
		chars := 0
		for _, block := range message.Assistant.Content {
			switch block := block.(type) {
			case ai.TextContent:
				chars += jsLength(block.Text)
			case ai.ThinkingContent:
				chars += jsLength(block.Thinking)
			case ai.ToolCall:
				chars += jsLength(block.Name) + ai.JSONStringifyLength(block.Arguments)
			}
		}
		return ceilQuarter(chars)
	case message.ToolResult != nil:
		return ceilQuarter(contentBlocksChars(message.ToolResult.Content))
	case message.Custom != nil:
		switch role, _ := message.Custom["role"].(string); role {
		case agent.RoleCustom:
			return ceilQuarter(customContentChars(message.Custom["content"]))
		case agent.RoleBashExecution:
			command, _ := message.Custom["command"].(string)
			output, _ := message.Custom["output"].(string)
			return ceilQuarter(jsLength(command) + jsLength(output))
		case agent.RoleBranchSummary, agent.RoleCompactionSummary:
			summary, _ := message.Custom["summary"].(string)
			return ceilQuarter(jsLength(summary))
		}
	}
	return 0
}

// getAssistantUsage returns an assistant message's usage when it is valid:
// not aborted or errored, and with a nonzero context size.
func getAssistantUsage(message agent.AgentMessage) *ai.Usage {
	assistant := message.Assistant
	if assistant == nil || assistant.Usage == nil || assistant.StopReason == ai.StopReasonAborted || assistant.StopReason == ai.StopReasonError {
		return nil
	}
	if agent.CalculateContextTokens(*assistant.Usage) <= 0 {
		return nil
	}
	return assistant.Usage
}

// EstimateContextTokens estimates context size from the last valid assistant
// usage plus an estimate of the messages after it, or from the messages alone
// when no usage exists (compaction.ts estimateContextTokens). LastUsageIndex
// is -1 for Pi's null.
func EstimateContextTokens(messages []agent.AgentMessage) agent.ContextUsageEstimate {
	// 从尾到头找最近一条"可信 usage"的 assistant 消息：
	// usage 是 provider 自报的准确值（比字符估算准），之后
	// 的消息只有尾部一小段，用字符估算补上即可——比逐条估
	// 全历史又准又快。
	for i, message := range slices.Backward(messages) {
		usage := getAssistantUsage(message)
		if usage == nil {
			continue
		}
		usageTokens := agent.CalculateContextTokens(*usage)
		trailing := estimateMessagesTokens(messages[i+1:])
		return agent.ContextUsageEstimate{
			Tokens:         usageTokens + trailing,
			UsageTokens:    usageTokens,
			TrailingTokens: trailing,
			LastUsageIndex: i,
		}
	}
	// 整条历史都没有可信 usage（如新会话首轮前）：全字符估算。
	estimated := estimateMessagesTokens(messages)
	return agent.ContextUsageEstimate{Tokens: estimated, TrailingTokens: estimated, LastUsageIndex: -1}
}

// EstimateProjectedContextTokens estimates a projected context without
// trusting usage captured before a later context edit or compaction on the
// branch (compaction.ts estimateProjectedContextTokens). Without trusted
// usage, it counts the replayed current system state once plus every
// non-system projected message.
//
// 可信度判定：provider 自报的 usage 只在"产出该 usage 的条目位于
// 分支上最后一次 context_edit/compaction 之后"时才可信——否则那次
// 编辑/压缩已改变了上下文内容，旧 usage 描述的是另一份历史。不可信
// 时退回逐条估算（当前系统态算一次 + 每条非系统消息 chars/4）。
func EstimateProjectedContextTokens(projection codingagent.SessionProjection, branchEntries []codingagent.SessionEntry) agent.ContextUsageEstimate {
	estimate := EstimateContextTokens(projection.Messages)
	if estimate.LastUsageIndex >= 0 {
		// 定位"最后可信 usage"对应的分支条目：投影消息序号
		// → 条目 ID → 分支下标（两段映射）。
		usageEntryID := ""
		projectedIndex := 0
		for _, entry := range projection.Entries {
			next := projectedIndex + len(entry.Messages)
			if estimate.LastUsageIndex < next {
				usageEntryID = entry.SourceEntry.Base().ID
				break
			}
			projectedIndex = next
		}
		usageEntryIndex := -1
		if usageEntryID != "" {
			usageEntryIndex = slices.IndexFunc(branchEntries, func(entry codingagent.SessionEntry) bool {
				return entry.Base().ID == usageEntryID
			})
		}
		// 从尾部找最近一次"使 usage 失真"的条目：compaction
		// 必失真（它替换了 usage 之前的内容）；context_edit
		// 只在目标条目位于 usage 锚点之前时才失真——D136：
		// 编辑锚点之后的条目（如抠掉失败重试）不影响锚点。
		latestInvalidating := -1
		for i, entry := range slices.Backward(branchEntries) {
			if entry.Base().Type == "compaction" {
				latestInvalidating = i
				break
			}
			if entry.Base().Type != "context_edit" {
				continue
			}
			// pig divergence (D136): an edit of an entry after the usage anchor (such as the omission of a failed retry attempt) leaves the anchor valid; upstream drops it for any later edit and re-estimates the whole history.
			if usageEntryIndex >= 0 && contextEditTargetsAfter(branchEntries, usageEntryIndex, entry) {
				continue
			}
			latestInvalidating = i
			break
		}
		if usageEntryIndex > latestInvalidating {
			return estimate
		}
	}
	// 无可用 usage 或已失真：逐条估算——当前系统态算一次，
	// 每条非系统消息 chars/4。
	tokens := 0
	if current := codingagent.CurrentSystemMessage(projection.Messages); current != nil {
		tokens = estimateSystemTokens(*current)
	}
	for _, message := range projection.Messages {
		if message.Role() != "system" {
			tokens += EstimateTokens(message)
		}
	}
	return agent.ContextUsageEstimate{Tokens: tokens, TrailingTokens: tokens, LastUsageIndex: -1}
}

// contextEditTargetsAfter reports whether a context_edit entry targets a branch entry after index.
func contextEditTargetsAfter(branchEntries []codingagent.SessionEntry, index int, edit codingagent.SessionEntry) bool {
	var parsed codingagent.ContextEditEntry
	if json.Unmarshal(edit.Raw(), &parsed) != nil {
		return false
	}
	target := slices.IndexFunc(branchEntries, func(entry codingagent.SessionEntry) bool { return entry.Base().ID == parsed.TargetID })
	return target > index
}
