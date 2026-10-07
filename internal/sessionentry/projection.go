package sessionentry

import (
	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/extensions/sdk/json"
)

// SessionTreeNode is a defensive copy of the session's branch
// structure for the /tree overlay (follow-up).
type SessionTreeNode struct {
	Entry    SessionEntry
	Children []*SessionTreeNode
	Label    string
	// LabelTimestamp is the on-disk timestamp of the LabelEntry that
	// set Label, in upstream's ISO-8601-ish wire format. Empty when
	// no label is set. Used by the /tree picker to render `hh:mm`
	// next to the label when the user toggles label timestamps on
	// (mirrors upstream tree-selector.ts:678-682).
	LabelTimestamp string
}

// SessionContextModel is the model identity last selected on a branch.
type SessionContextModel struct {
	Provider string `json:"provider"`
	ModelID  string `json:"modelId"`
}

// ProjectedSessionEntry pairs a raw append-only entry with the model-visible
// messages it contributes after context edits (session-manager.ts
// ProjectedSessionEntry). Messages is empty for state-only entries and
// omissions.
type ProjectedSessionEntry struct {
	SourceEntry SessionEntry
	Messages    []agent.AgentMessage
}

// SessionProjection is the provenance-preserving, compaction-aware model
// context of one branch (session-manager.ts SessionProjection).
//
// 投影 = "磁盘上的条目序列"投影成"模型实际看到的消息序列"，
// 应用三层语义：
//  1. 只取最新压缩点之后的范围（更早的历史只剩摘要文本）；
//  2. context_edit 按 targetId 覆盖/删除对应条目产出的消息；
//  3. 每种条目类型映射到其上下文消息形态（compaction→摘要+系统
//     快照，branch_summary→摘要消息，custom_message→custom 角色等），
//     纯状态条目（model_change/label/session_info 等）投影为空。
//
// Entries 保留每条来源条目的指针，供"消息→磁盘条目"反查
// （findPersistedMessageEntryID 就靠它）。
type SessionProjection struct {
	Entries  []ProjectedSessionEntry
	Messages []agent.AgentMessage
	// ThinkingLevel and Model are the latest settings on the projected branch; ThinkingLevel is "off" and Model nil when the branch has none.
	ThinkingLevel string
	Model         *SessionContextModel
	// HasContextEdits reports that Entries holds a context_edit entry: usage recorded before the edit no longer describes the projected context.
	HasContextEdits bool
}

// ContextEditableContent is the content a context edit may replace: a user, assistant, tool-result or custom message's content, a JSON string or block array. It stays raw JSON until the projection decodes it for the target's role.
// upstream: session-manager.ts:168 ContextEditableContent
type ContextEditableContent = json.RawMessage

// ContextEditReplacement is the non-null replacement of a context_edit entry
// (session-manager.ts ContextEditEntry.replacement). Content is the JSON
// string or content-block array that replaces the target's content
// (ContextEditableContent).
type ContextEditReplacement struct {
	Content ContextEditableContent `json:"content"`
}

// SessionContext contains model-visible messages and the settings on their full branch path.
type SessionContext struct {
	Messages      []agent.AgentMessage `json:"messages"`
	ThinkingLevel string               `json:"thinkingLevel"`
	Model         *SessionContextModel `json:"model"`
}

// NewSessionOptions are the options of Session.NewSession. A nil ID generates one.
// Mirrors upstream NewSessionOptions (core/session-manager.ts:52).
type NewSessionOptions struct {
	ID            *string
	ParentSession string
}
