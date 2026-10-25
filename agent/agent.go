// Package agent provides the core agent loop and message types.
// Mirrors packages/agent of the pinned Pi release.
//
// 本包是整個 agent 运行机制的最底层：消息类型（AgentMessage 及其变体）、
// 事件流（AgentEvent 各子类型）、工具接口（AgentTool）、以及驱动
// "请求模型 → 流式收答 → 执行工具 → 再请求" 这一循环的 Agent 类型。
//
// 典型使用流程：
//  1. 上层（Session / codingagent）用 NewAgent(AgentOptions{...}) 构造 Agent；
//  2. 用户输入经 Send/SendContent 进入，占用运行权后调用 runLoop；
//  3. runLoop 每个 turn 调用 streamAssistantResponse 向 provider 发请求，
//     consumeStream 把流式事件转成 message_start/update/end 与 assistant 消息；
//  4. 若回复含 tool_call，executeToolCalls 批量执行工具并把 toolResult
//     消息追加回上下文，然后进入下一个 turn；
//  5. 全程通过 emit 发出 AgentEvent，供 TUI、持久化、扩展钩子观察。
//
// 包内文件分工：agent.go 放公共类型与 Agent 生命周期；agent_loop.go 放主
// 循环；tool_execution.go / run_tool_call.go 放工具执行管线；queue.go 放
// steering/follow-up 队列；lifecycle.go 放订阅、取消与运行信号；
// messages.go / message_json.go / transform.go 负责消息与 LLM 线格式互转；
// validate*.go 是工具参数的 TypeBox 语义校验；proxy.go 是代理流实现。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/internal/jsloop"
)

// ─── Message Types ────────────────────────────────────────────────────────────

// Role constants mirror upstream.
//
// role 是消息在 JSON 线上的判别字段：序列化后的消息不再有 Go 类型信息，
// 只凭 role 字符串区分（见 message_json.go 的 UnmarshalJSON）。前三个是
// 模型协议角色；后四个是会话层自定义角色（bash 执行记录、扩展自定义消息、
// 分支摘要、压缩摘要），它们在发给模型前会被 ConvertToLLM 改写成 user 消息。
const (
	RoleUser       = "user"
	RoleAssistant  = "assistant"
	RoleToolResult = "toolResult"
	// Custom roles used by the coding agent layer.
	RoleBashExecution     = "bashExecution"
	RoleCustom            = "custom"
	RoleBranchSummary     = "branchSummary"
	RoleCompactionSummary = "compactionSummary"
)

// UserMessage is a user turn whose content retains the upstream string-or-block-array shape.
//
// 用户一轮输入的消息。Content 既可以是纯文本（ai.UserText）也可以是
// 文本/图片块数组（ai.UserContentBlocks），因为线上格式允许两种写法，
// 反序列化时按 JSON 首字符是 `"` 还是 `[` 区分。
type UserMessage struct {
	Role      string         `json:"role"` // "user"
	Content   ai.UserContent `json:"content"`
	Timestamp int64          `json:"timestamp"`

	// source is the object this message was decoded from, when its JSON.stringify text differs from the text the fields encode to. A pointer keeps the struct one word larger: messages are copied by value on every context build.
	source *messageSource
}

// AssistantMessage is a streamed response from the LLM. Streaming events retain a shallow provider view; Observe and JSON serialization read its current nested state without mutating these exported fields.
//
// 模型一次回复的消息。它有两种形态：流式进行中的"浅视图"（streamView
// 非空，Content 等字段可能滞后于 provider 内部状态，读取要走 Observe）
// 和已完成的定稿消息（streamView 为空，字段即最终值）。
// StopReason 是理解整个循环的关键字段：stop=正常结束、toolUse=有工具
// 调用待执行（循环继续）、aborted/error=硬退出。ErrorMessage 仅在
// StopReason=="error" 时携带失败原因，供会话层做重试分类。
type AssistantMessage struct {
	Role                  string                     `json:"role"` // "assistant"
	Content               []ai.AssistantContentBlock `json:"content"`
	Thinking              string                     `json:"-"`
	Timestamp             int64                      `json:"timestamp"`
	Usage                 *ai.Usage                  `json:"usage,omitempty"`
	API                   ai.API                     `json:"api,omitempty"`
	Provider              string                     `json:"provider,omitempty"`
	ModelID               string                     `json:"model,omitempty"`
	ResponseModel         string                     `json:"responseModel,omitempty"`
	ResponseID            string                     `json:"responseId,omitempty"`
	ProviderThinkingLevel string                     `json:"providerThinkingLevel,omitempty"`
	// ThinkingLevel is the level the agent loop requested for this response; see ai.AssistantMessage.ThinkingLevel.
	ThinkingLevel ai.ModelThinkingLevel           `json:"thinkingLevel,omitempty"`
	Diagnostics   []ai.AssistantMessageDiagnostic `json:"diagnostics,omitempty"`
	Deferred      *ai.DeferredHandle              `json:"deferred,omitempty"`
	// StopReason records why the assistant turn ended. Mirrors upstream
	// AssistantMessage.stopReason in packages/ai/src/types.ts.
	// Values: "stop" (normal), "toolUse" (tool calls present),
	//         "aborted" (context cancelled), "error" (EventError received).
	StopReason ai.StopReason `json:"stopReason,omitempty"`
	// ErrorMessage holds the error string when StopReason == "error".
	ErrorMessage string `json:"errorMessage,omitempty"`
	// ErrorBeforeTimestamp records that the message was built with errorMessage before timestamp; see ai.AssistantMessage.ErrorBeforeTimestamp.
	ErrorBeforeTimestamp bool   `json:"-"`
	RawStopReason        string `json:"rawStopReason,omitempty"`
	EndTurn              *bool  `json:"endTurn,omitempty"`
	// DurationMs is the milliseconds from the start of the request to the final message, measured with a monotonic clock by the AssistantMessageEventStream. Nil for legacy messages and for deferred results fetched later.
	// upstream: ai/src/types.ts:582 AssistantMessage.durationMs
	DurationMs *int64 `json:"durationMs,omitempty"`
	// ThinkingSignature is a legacy runtime convenience; durable signatures
	// live on ThinkingContent blocks in the upstream wire shape.
	ThinkingSignature string `json:"-"`

	// streamView pins top-level values and retains the provider's nested object identities.
	streamView *ai.AssistantMessage
	// streamSeq is the stream publication (ai.AssistantMessage.Publication) the exported fields were taken at; refreshed keeps the message while the stream has published nothing since.
	streamSeq uint64
	// source is the object this message was decoded from, when its JSON.stringify text differs from the text the fields encode to. A pointer keeps the struct one word larger: messages are copied by value on every context build.
	source *messageSource
}

// ToolResultMessage carries one tool execution result back to the LLM.
// Mirrors upstream pi-ai ToolResultMessage.
//
// 一次工具执行的结果消息。每个 assistant 消息里的 tool_call 块都必须有
// 对应的 ToolResultMessage 紧随其后，否则 provider 会拒绝请求（transform.go
// 的 NormalizeMessages 负责补洞）。DetailsNull 用来区分"工具没写 details"
// （JSON 里整个键省略）与"工具显式写了 details: null"——两者在线上
// 是不同的字节序列，会话持久化必须原样还原。
type ToolResultMessage struct {
	Role       string                        `json:"role"` // "toolResult"
	ToolCallID string                        `json:"toolCallId"`
	ToolName   string                        `json:"toolName"`
	Content    []ai.ToolResultMessageContent `json:"content"`
	// Details is the tool's details value. Nil is no details unless DetailsNull is set.
	Details any `json:"details,omitempty"`
	// DetailsNull marks a nil Details as an explicit JSON null: the tool wrote details: null, and JSON writes
	// `"details":null`. Without it a nil Details is absent and JSON omits the key. It has no effect when Details is set.
	DetailsNull bool      `json:"-"`
	Usage       *ai.Usage `json:"usage,omitempty"`
	// NestedCalls are the calls this tool made to other tools. Kept for the session record; not sent to the model.
	// upstream: .upstream/v0.99.1/packages/ai/src/types.ts:604
	NestedCalls *ai.NestedToolCalls `json:"nestedCalls,omitempty"`
	IsError     bool                `json:"isError"`
	// DurationMs is the milliseconds execute() took, measured with a monotonic clock. Nil when the tool did not run, and for
	// results stored before it was recorded. upstream: ai/src/types.ts ToolResultMessage.durationMs
	DurationMs *int64 `json:"durationMs,omitempty"`
	Timestamp  int64  `json:"timestamp"`

	// source is the object this message was decoded from, when its JSON.stringify text differs from the text the fields encode to. A pointer keeps the struct one word larger: messages are copied by value on every context build.
	source *messageSource
}

func (m ToolResultMessage) Text() string {
	var text strings.Builder
	for _, block := range m.Content {
		if value, ok := block.(ai.TextContent); ok {
			text.WriteString(value.Text)
		}
	}
	return text.String()
}

// Result is the result a display renders for this message: its content, details and error flag. A message with
// details: null gives a result that holds details: null.
func (m ToolResultMessage) Result() AgentToolResult {
	result := AgentToolResult{Content: m.Content, Details: m.Details, IsError: m.IsError}
	if m.Details == nil && m.DetailsNull {
		result.MemberOrder = []string{"content", "details"}
	}
	return result
}

func (m ToolResultMessage) Images() []ai.ImageContent {
	var images []ai.ImageContent
	for _, block := range m.Content {
		if value, ok := block.(ai.ImageContent); ok {
			images = append(images, value)
		}
	}
	return images
}

// AgentMessage is the union of all message types. Only one field is non-nil.
//
// 会话中所有消息的联合信封：同一时刻只有一个字段非空，相当于 TypeScript
// 里的判别联合类型（Go 没有联合，用"多指针字段+至多一个非空"表达）。
// Custom 兜住所有自定义角色，以原始 map 保存扩展写入的任意字段。
// MarshalJSON/UnmarshalJSON 通过 role 字段把它拍平回线上格式。
type AgentMessage struct {
	System     *ai.SystemMessage
	User       *UserMessage
	Assistant  *AssistantMessage
	ToolResult *ToolResultMessage
	// Custom message types (bash execution, branch summary, etc.)
	Custom map[string]any // raw JSON for custom types

	// source is the object a System or Custom message was decoded from, when its JSON.stringify text differs from the text the fields encode to. Custom encodes its current members in the order of the decoded object.
	source *messageSource
}

// messageSource is the JSON.stringify text of the object a message was decoded from.
//
// 反序列化时留下的原文（JSON.stringify 后的字节）：上游 Pi 保留解析
// 出的对象并原样回写，成员顺序、未建模字段、数字拼写都不丢——
// SourceJSON 用它比对决定"回写原文还是按当前字段重编码"。
type messageSource struct {
	text []byte
	// nullContent reports that the object's content member is null or missing.
	nullContent bool
}

// Role returns the role string.
func (m AgentMessage) Role() string {
	switch {
	case m.System != nil:
		return "system"
	case m.User != nil:
		return m.User.Role
	case m.Assistant != nil:
		return m.Assistant.Role
	case m.ToolResult != nil:
		return m.ToolResult.Role
	case m.Custom != nil:
		if r, ok := m.Custom["role"].(string); ok {
			return r
		}
	}
	return ""
}

// MessageRole is the role property of the message (ai.RoleMessage): an agent transcript goes to the transcript helpers as it is.
//
// ai 包转录辅助函数需要的 role 适配：AgentMessage 直接以自身 role 应答。
func (m AgentMessage) MessageRole() string { return m.Role() }

// AsSystemMessage returns the wrapped system message, which the transcript helpers read when the role is "system".
//
// role 为 "system" 时把信封拆出 SystemMessage 供转录辅助函数读取。
func (m AgentMessage) AsSystemMessage() (ai.SystemMessage, bool) {
	if m.System == nil {
		return ai.SystemMessage{}, false
	}
	return *m.System, true
}

func (m AgentMessage) ContentBlocks() []ai.ContentBlock {
	var blocks []ai.ContentBlock
	switch {
	case m.System != nil:
		switch content := m.System.Content.(type) {
		case ai.SystemText:
			blocks = []ai.ContentBlock{ai.TextContent{Text: string(content)}}
		case ai.SystemTextBlocks:
			blocks = make([]ai.ContentBlock, len(content))
			for i, block := range content {
				blocks[i] = block
			}
		}
	case m.User != nil:
		switch content := m.User.Content.(type) {
		case ai.UserText:
			blocks = []ai.ContentBlock{ai.TextContent{Text: string(content)}}
		case ai.UserContentBlocks:
			blocks = make([]ai.ContentBlock, len(content))
			for i, block := range content {
				blocks[i] = block
			}
		}
	case m.Assistant != nil:
		message := m.Assistant.Observe()
		blocks = make([]ai.ContentBlock, len(message.Content))
		for i, block := range message.Content {
			blocks[i] = block
		}
	case m.ToolResult != nil:
		blocks = make([]ai.ContentBlock, len(m.ToolResult.Content))
		for i, block := range m.ToolResult.Content {
			blocks[i] = block
		}
	}
	return blocks
}

// ─── Tool Interface ───────────────────────────────────────────────────────────

// ToolUpdateCallback is called when a tool emits a streaming progress update. The partial result is the complete-so-far AgentToolResult, which the tool_execution_update event carries verbatim as `partialResult`.
// upstream: packages/agent/src/types.ts AgentToolUpdateCallback, agent-loop.ts:778-786
//
// 传给工具 Execute 的进度回调。注意契约是"累计快照"而非增量：每次调用
// 携带的都是到目前为止的完整结果，TUI 直接整体替换显示。工具可以在自己
// 的 goroutine 里调用它。
type ToolUpdateCallback func(partial AgentToolResult)

// ToolUpdateSink receives the progress updates of a tool call that [RunToolCall] runs. A non-nil error is upstream's rejected promise: the update is not delivered further, later updates still reach the sink, and the call fails with the first error once the tool returns.
//
// upstream: agent-loop.ts:685 (ToolUpdateSink), 820-849 (executePreparedToolCall)
type ToolUpdateSink = func(partial AgentToolResult) error

// ToolExecutionMode controls parallelism.
type ToolExecutionMode string

const (
	ToolModeSequential ToolExecutionMode = "sequential"
	ToolModeParallel   ToolExecutionMode = "parallel"
)

// AgentToolResult carries the ordered text/image blocks returned by a tool.
// Ports packages/agent/src/types.ts.
//
// 工具执行的返回值。三个容易混淆的标志位：
//   - IsError：工具主动报告的业务失败（如命令退出码非 0），结果保留
//     details/structuredContent；
//   - Thrown：工具抛出异常（Go 里等价于 Execute 返回 error 或 panic），
//     结果会被换成只有错误文本的 createErrorToolResult 形态；
//   - Terminate：提示 agent 在这一批工具后停止运行——只有整批结果都
//     置位才真正提前结束（见 shouldTerminateToolBatch）。
//
// MemberOrder 记录工具写入字段的原始顺序，因为 JSON 序列化要保持和
// 上游对象成员顺序一致的字节输出（持久化文件逐字节比对依赖它）。
type AgentToolResult struct {
	// MemberOrder names the members of the result object in the order its tool wrote them (content, details,
	// isError, structuredContent, usage, terminate). Upstream hands the tool's own object on, so JSON.stringify
	// writes its members in that order; a result without it (a built-in tool, a Go tool) has the declared order.
	// upstream: agent-loop.ts:778-786, 912-919
	MemberOrder []string
	Content     []ai.ToolResultMessageContent
	Details     any
	// StructuredContent is the machine-readable result of a tool that declares
	// an output schema. upstream: structuredContent?: JsonValue
	StructuredContent json.RawMessage
	// IsError is upstream's `result.isError`: the tool returned a failure instead of throwing it, so
	// details and structuredContent are kept. A thrown error, an unknown tool, invalid arguments and a
	// blocked call produce a result without it; the failure is reported by ToolExecutionEndEvent.IsError
	// and AgentToolCallOutcome.IsError. upstream: types.ts:436-441, agent-loop.ts:906-910
	IsError bool
	// Thrown marks a result that stands for an error upstream's tool throws. The agent reports the call as failed, drops
	// the result's IsError and Thrown, and keeps the rest, which is createErrorToolResult's content and empty details
	// (agent-loop.ts:906-910). A tool that returns a Go error has the same effect.
	Thrown bool
	// StructuredContentAppended records that afterToolCall gave a result that had no structuredContent one. Upstream's
	// `{...result}` spread keeps the tool's key order and assigning an existing key keeps its position, so only a key the
	// hook adds follows every key the tool set (agent-loop.ts:877-889). It only orders the serialized result.
	StructuredContentAppended bool
	// Usage is the usage of the tool execution itself, if available. It is
	// not used for main LLM context accounting. upstream: usage?: Usage
	Usage *ai.Usage
	// Terminate hints that the agent should stop after the current tool
	// batch. Early termination happens only when every finalized result in
	// the batch sets it. upstream: terminate?: boolean
	Terminate bool
	// pig additive (D160): the collapsed-view preview of a subprocess extension's tool result.
	// Preview is an optional short summary shown when the tool result is
	// collapsed in the TUI. When non-empty, overrides the default
	// tail-preview behavior. Extensions can set this to provide a
	// custom collapsed view (e.g. a table header, a line count, or a
	// brief summary) without implementing a full BodyRenderer.
	// pig extension: no upstream equivalent (Pi extensions use in-process
	// renderResult callbacks; subprocess extensions need a serializable
	// alternative).
	Preview string
}

// MemberNames lists the members of the result object this result holds (content, details, structuredContent, isError,
// usage, terminate), in the order upstream's object keeps them. Without a recorded MemberOrder that is the order the
// built-in tools build theirs in, with a structuredContent a hook added last. With one it is the tool's own order, and a
// member the tool did not set, which a hook added, follows the tool's in the order of the hook's spread, `{...result,
// content, details, usage, terminate}`, then the structuredContent assigned after it (agent-loop.ts:877-889). Content is
// always listed. A member MemberOrder names is held even when its value is a zero value (details or usage null, isError or
// terminate false): the tool wrote it, so upstream's object has it and JSON.stringify writes it.
// upstream: agent-loop.ts:778-786, 877-889, 912-919; coding-agent/src/core/tools/bash.ts:403-409
func (r AgentToolResult) MemberNames() []string {
	written := func(name string) bool { return slices.Contains(r.MemberOrder, name) }
	held := map[string]bool{
		"content":           true,
		"details":           r.Details != nil || written("details"),
		"structuredContent": len(r.StructuredContent) > 0,
		"isError":           r.IsError || written("isError"),
		"usage":             r.Usage != nil || written("usage"),
		"terminate":         r.Terminate || written("terminate"),
	}
	var names []string
	add := func(name string) {
		if held[name] && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	if len(r.MemberOrder) == 0 {
		for _, name := range []string{"content", "details"} {
			add(name)
		}
		if !r.StructuredContentAppended {
			add("structuredContent")
		}
		for _, name := range []string{"isError", "usage", "terminate", "structuredContent"} {
			add(name)
		}
		return names
	}
	for _, name := range r.MemberOrder {
		add(name)
	}
	for _, name := range []string{"content", "details", "usage", "terminate", "structuredContent", "isError"} {
		add(name)
	}
	return names
}

// DetailsNull reports a result that holds details: null: the tool wrote the details member with no value. A Go tool
// that leaves Details nil and records no MemberOrder holds no details.
func (r AgentToolResult) DetailsNull() bool {
	return r.Details == nil && slices.Contains(r.MemberNames(), "details")
}

// Text joins text blocks for text-only displays; Content retains their boundaries and image positions.
func (r AgentToolResult) Text() string {
	var text []string
	for _, block := range r.Content {
		if value, ok := block.(ai.TextContent); ok {
			text = append(text, value.Text)
		}
	}
	return strings.Join(text, "\n")
}

// Images selects image blocks for displays with a separate image area.
func (r AgentToolResult) Images() []ai.ImageContent {
	return (ToolResultMessage{Content: r.Content}).Images()
}

// AgentTool is the interface that all tools must implement.
//
// 一切可被模型调用的工具（内置 bash/read/edit、扩展注册工具等）的抽象。
// Schema() 返回的 JSON Schema 有两处用途：放进系统消息的工具声明（让模型
// 知道怎么调），以及在 prepareToolCall 里校验模型发来的参数。
// ExecutionMode() 决定并行批次策略：声明 sequential 的工具会迫使整个
// 批次退化为顺序执行（见 executeToolCalls）。
type AgentTool interface {
	// Name is the tool's name as known to the LLM.
	Name() string
	// Label returns a human-readable display name for the tool header.
	// If empty, Name() is used for display. Mirrors upstream AgentTool.label.
	Label() string
	// Schema returns the JSON Schema for the tool's parameters.
	Schema() ai.ToolSchema
	// Execute runs the tool.
	Execute(ctx context.Context, toolCallID string, params json.RawMessage,
		onUpdate ToolUpdateCallback) (AgentToolResult, error)
	// ExecutionMode returns the tool's parallelism preference.
	ExecutionMode() ToolExecutionMode
}

// ExtensionTool is implemented by a tool whose Execute runs in an extension runtime, on an event loop of its own. Pi's loop turns during such a tool only if the tool awaits real work, which the host cannot tell, so the agent opens no wait for it (jsloop.Turns).
//
// 标记接口：Execute 跑在扩展运行时（JS 事件循环）内的工具。上游只有
// 在工具真的 await 异步工作时事件循环才空转，但宿主无法分辨这种
// "假 await"，所以 agent 侧对这类工具不开 jsloop.Turns 等待。
type ExtensionTool interface {
	// RunsInExtension reports whether the tool's Execute runs in an extension; a wrapper reports its wrapped tool's answer.
	RunsInExtension() bool
}

// ArgumentPreparer is an optional interface tools may implement to
// normalize / coerce their raw input BEFORE schema validation runs.
// Mirrors upstream's per-tool `prepareArguments` hook
// (.upstream/current/packages/coding-agent/src/core/tools/edit.ts:308,
// `parameters: editSchema, ... prepareArguments: prepareEditArguments`).
//
// Use cases:
//   - edit tool: rewrites the legacy `{path, oldText, newText}` flat
//     input shape into `{path, edits:[{oldText, newText}]}` so the
//     strict editSchema accepts it (LLMs like Opus 4.6 / GLM-5.1 still
//     emit the flat shape).
//   - any future tool that accepts multiple wire shapes for the same
//     semantic input.
//
// Without this hook, schema validation in dispatch() rejects valid
// legacy inputs before tool.Execute() ever runs, breaking parity with
// upstream which normalizes first and validates after.
type ArgumentPreparer interface {
	PrepareArguments(raw json.RawMessage) (json.RawMessage, error)
}

// ─── Agent Events ─────────────────────────────────────────────────────────────

// AgentEvent is emitted by the agent loop to notify observers.
//
// 事件是 agent 对外界唯一的数据出口：TUI 刷新、会话持久化、扩展钩子、
// 遥测全靠订阅它。密封接口（agentEvent 私有方法）保证事件集合是封闭的，
// 外部只能消费不能新增。一次完整运行的事件序列为：
//
//	agent_start
//	  ├─ turn_start → message_start/update…/message_end（assistant 流式回复）
//	  │             → tool_execution_start/update/end × N（工具批次）
//	  │             → message_start/end（每条 toolResult 消息）
//	  │             → turn_end
//	  └─（有工具调用或排队消息则重复上一行）
//	agent_end
//
// 任何监听器返回错误都会以 runFailure panic 终止本次运行（见 emit）。
type AgentEvent interface{ agentEvent() }

// AgentStartEvent 标志一次运行的开始；AgentEndEvent 标志结束并携带本次
// 运行产生的全部新消息（WillRetry 提示会话层还会自动重试）。
type AgentStartEvent struct{}
type AgentEndEvent struct {
	Messages  []AgentMessage
	WillRetry bool
}

// AgentSettledEvent fires once an agent run has fully settled: no automatic
// retry, compaction, or queued continuation will follow. Upstream emits it from
// the _runAgentPrompt finally (agent-session.ts:1073) to both the extension
// runner and the session event stream.
type AgentSettledEvent struct {
	// Aborted is whether the run ended because it was aborted, for example with Escape (agent-session.ts:203).
	Aborted bool
}

// QueueUpdateEvent reports the current pending steering and follow-up text.
// It mirrors the coding-agent session queue_update event.
type QueueUpdateEvent struct {
	Steering []string
	FollowUp []string
}

// SessionInfoChangedEvent reports the effective Session name; an empty name clears it.
type SessionInfoChangedEvent struct {
	Name string
}

// BashExecutionUpdateEvent reports one command-output delta. A nil ID omits the correlation identifier; a non-nil empty ID remains explicit.
type BashExecutionUpdateEvent struct {
	ID    *string
	Delta string
}

// ThinkingLevelChangedEvent reports an effective reasoning-level change.
type ThinkingLevelChangedEvent struct {
	Level ai.ModelThinkingLevel
}

type TurnStartEvent struct {
	TurnIndex int
	Timestamp time.Time
}
type TurnEndEvent struct {
	TurnIndex          int
	Message            AgentMessage
	ToolResults        []ToolResultMessage
	MessageEntryID     string
	ToolResultEntryIDs []string
}
type MessageStartEvent struct {
	Message     AgentMessage
	observation *ai.StreamObservation
}
type MessageUpdateEvent struct {
	Message               AgentMessage
	AssistantMessageEvent ai.AssistantMessageEvent
	observation           *ai.StreamObservation
}
type MessageEndEvent struct {
	Message     AgentMessage
	observation *ai.StreamObservation
}

// RefreshEvent returns the message event with the assistant message's and the provider event's partial fields set to the stream state at this call. A host dispatcher calls it when it delivers a retained event to a listener after an awaited step. Other events are returned unchanged.
//
// 事件保鲜：消息事件里携带的 assistant 浅视图是发出瞬间的快照，若
// 派发途中有 await 间隔（比如 Session 逐个通知监听器），先调它把
// partial 刷新成"此刻"状态再投递，后到的监听器看到的才是新内容。
// RefreshEvent returns the message event with the assistant message's and the provider event's partial fields set to the stream state at this call. A host dispatcher calls it when it delivers a retained event to a listener after an awaited step. An event whose message and partial already hold that state (the stream published nothing since they were taken), and other events, are returned unchanged.
func RefreshEvent(event AgentEvent) AgentEvent {
	switch value := event.(type) {
	case MessageStartEvent:
		if assistant := value.Message.Assistant.refreshed(); assistant != value.Message.Assistant {
			value.Message.Assistant = assistant
			return value
		}
	case MessageUpdateEvent:
		assistant := value.Message.Assistant.refreshed()
		providerEvent := value.AssistantMessageEvent
		if providerEvent != nil {
			providerEvent = ai.RefreshEvent(providerEvent)
		}
		if assistant != value.Message.Assistant || assistantEventPartial(providerEvent) != assistantEventPartial(value.AssistantMessageEvent) {
			value.Message.Assistant = assistant
			value.AssistantMessageEvent = providerEvent
			return value
		}
	case MessageEndEvent:
		if assistant := value.Message.Assistant.refreshed(); assistant != value.Message.Assistant {
			value.Message.Assistant = assistant
			return value
		}
	}
	return event
}

// assistantEventPartial returns the partial of the provider events ai.RefreshEvent refreshes, nil for the others. ai.RefreshEvent returns an event whose partial it keeps as it is, so comparing partials tells whether it refreshed one.
//
// 取出会被 ai.RefreshEvent 刷新的事件里那个 partial：刷新返回的事件
// partial 不变，因此比较 partial 指针就能判断事件是否被刷新过。
func assistantEventPartial(event ai.AssistantMessageEvent) *ai.AssistantMessage {
	if start, ok := event.(ai.StartEvent); ok {
		return start.Partial
	}
	partial, _ := assistantUpdatePartial(event)
	return partial
}

// EventObservation returns the attached stream scope for synchronous host dispatch. Session transfers that scope before notifying its synchronous subscribers; it is not message data or wire metadata.
func EventObservation(event AgentEvent) *ai.StreamObservation {
	switch event := event.(type) {
	case MessageStartEvent:
		return event.observation
	case MessageUpdateEvent:
		return event.observation
	case MessageEndEvent:
		return event.observation
	default:
		return nil
	}
}

// WithEventObservation carries a stream scope across synchronous host dispatch without changing message identity or serialized fields. Events outside the message lifecycle are unchanged.
//
// 给消息生命周期事件挂上流观察域（私有不序列化字段）：Session 在
// 同步派发时要靠它把事件接回正确的微任务/回调顺序上，与 RefreshEvent
// 是一对（一个挂域，一个按域刷新）。
func WithEventObservation(event AgentEvent, observation *ai.StreamObservation) AgentEvent {
	switch value := event.(type) {
	case MessageStartEvent:
		value.observation = observation
		return value
	case MessageUpdateEvent:
		value.observation = observation
		return value
	case MessageEndEvent:
		value.observation = observation
		return value
	default:
		return event
	}
}

type ToolExecutionStartEvent struct {
	ToolCallID string
	ToolName   string
	ToolLabel  string // human-readable display name (empty = use ToolName)
	Args       json.RawMessage
	// ParentToolCallID is set on the events of a call another tool made through `ctx.executeTool()`. upstream: agent-session.ts:182-189 (WithParentToolCallId)
	ParentToolCallID string
}
type ToolExecutionUpdateEvent struct {
	ToolCallID string
	ToolName   string
	// PartialResult is the tool's streaming progress. Subsequent updates carry the full accumulated result so far (upstream's onUpdate contract: every update is a complete-so-far snapshot, not a delta). Tool components in the TUI replace their visible body with each update, giving a live tail. upstream: agent-loop.ts:778-786
	PartialResult AgentToolResult
	// Args is the original tool call arguments (same as ToolExecutionStartEvent.Args).
	// Passed through for extensions that need to correlate updates with inputs.
	Args json.RawMessage
	// ParentToolCallID is set on the events of a call another tool made through `ctx.executeTool()`. upstream: agent-session.ts:182-189 (WithParentToolCallId)
	ParentToolCallID string
}
type ToolExecutionEndEvent struct {
	ToolCallID string
	ToolName   string
	Result     AgentToolResult
	// IsError is upstream's `event.isError`: the call failed, whether the tool threw, returned `isError`, or an
	// afterToolCall hook said so. It is not Result.IsError. upstream: agent-loop.ts:912-919
	IsError bool
	// DurationMs is the milliseconds the tool's execute() took, measured with a monotonic clock; nil when the tool did not run.
	// upstream: agent/src/types.ts tool_execution_end.durationMs
	DurationMs *int64
	// ParentToolCallID is set on the events of a call another tool made through `ctx.executeTool()`. upstream: agent-session.ts:182-189 (WithParentToolCallId)
	ParentToolCallID string
}

// TimingEvent retains the former diagnostic event shape for source compatibility.
// The agent no longer emits timing events, matching Pi's event stream.
//
// Deprecated: Read Agent.Timings or observe TurnEndEvent and ToolExecutionEndEvent.
type TimingEvent = timingEvent

type timingEvent struct {
	Kind     string
	Name     string
	Duration time.Duration
	Snapshot TimingSnapshot
}

func (timingEvent) agentEvent() {}

// CompactionStartEvent fires when auto-compaction or /compact begins.
// Mirrors upstream compaction_start event (agent-session.ts).
type CompactionStartEvent struct {
	Reason string // "manual" | "threshold" | "overflow"
}

// CompactionEndEvent fires when compaction completes, aborts, or errors.
// Mirrors upstream compaction_end event (agent-session.ts).
type CompactionEndEvent struct {
	Reason               string
	Aborted              bool
	WillRetry            bool
	Summary              string
	FirstKeptEntryID     string
	TokensBefore         int
	EstimatedTokensAfter int
	Usage                *ai.Usage
	Details              any
	ErrorMessage         string
}

func (AgentStartEvent) agentEvent()           {}
func (AgentEndEvent) agentEvent()             {}
func (AgentSettledEvent) agentEvent()         {}
func (BashExecutionUpdateEvent) agentEvent()  {}
func (QueueUpdateEvent) agentEvent()          {}
func (SessionInfoChangedEvent) agentEvent()   {}
func (ThinkingLevelChangedEvent) agentEvent() {}
func (TurnStartEvent) agentEvent()            {}
func (TurnEndEvent) agentEvent()              {}
func (MessageStartEvent) agentEvent()         {}
func (MessageUpdateEvent) agentEvent()        {}
func (MessageEndEvent) agentEvent()           {}
func (ToolExecutionStartEvent) agentEvent()   {}
func (ToolExecutionUpdateEvent) agentEvent()  {}
func (ToolExecutionEndEvent) agentEvent()     {}

// AutoRetryStartEvent fires when pig is about to retry a failed request
// due to a transient error (overload, rate limit, server error).
// Mirrors upstream auto_retry_start (agent-session.ts:130).
type AutoRetryStartEvent struct {
	Attempt      int
	MaxAttempts  int
	DelayMs      int
	ErrorMessage string
}

// AutoRetryEndEvent fires when an auto-retry cycle completes (success or
// exhausted). Mirrors upstream auto_retry_end (agent-session.ts:131).
type AutoRetryEndEvent struct {
	Success    bool
	Attempt    int
	FinalError string // set when Success==false
}

// SummarizationRetryScheduledEvent fires before the backoff sleep of each retry
// of a compaction or branch-summary summarization call. Mirrors upstream
// summarization_retry_scheduled (agent-session.ts:167).
type SummarizationRetryScheduledEvent struct {
	Attempt      int
	MaxAttempts  int
	DelayMs      int
	ErrorMessage string
}

// SummarizationRetryAttemptStartEvent fires after the backoff sleep, right
// before the retried summarization call. Source is "compaction" or
// "branchSummary"; Reason ("manual"|"threshold"|"overflow") applies to
// compaction and lets the TUI recreate the right status indicator. Mirrors
// upstream summarization_retry_attempt_start (agent-session.ts:173).
type SummarizationRetryAttemptStartEvent struct {
	Source string
	Reason string
}

// SummarizationRetryFinishedEvent fires once when a retried summarization loop
// ends. Mirrors upstream summarization_retry_finished (agent-session.ts:179).
type SummarizationRetryFinishedEvent struct{}

// EntryAppendedEvent reports a Session entry appended outside the agent loop,
// such as cache-warming usage. Entry is the persisted entry's JSON. Mirrors
// upstream AgentSessionEvent entry_appended (agent-session.ts:178).
type EntryAppendedEvent struct {
	Entry json.RawMessage
}

func (EntryAppendedEvent) agentEvent()                  {}
func (CompactionStartEvent) agentEvent()                {}
func (CompactionEndEvent) agentEvent()                  {}
func (AutoRetryStartEvent) agentEvent()                 {}
func (AutoRetryEndEvent) agentEvent()                   {}
func (SummarizationRetryScheduledEvent) agentEvent()    {}
func (SummarizationRetryAttemptStartEvent) agentEvent() {}
func (SummarizationRetryFinishedEvent) agentEvent()     {}

// ─── Before/After Hooks ───────────────────────────────────────────────────────

// ToolCallHookResult controls whether tool execution proceeds.
//
// Mirrors upstream BeforeToolCallResult. Terminate on a blocked call joins the
// batch early-termination rule: the agent stops after the batch only when
// every finalized result in it terminates.
//
// before 钩子的裁决结果：Block=true 直接拦截执行（生成错误结果而不调用
// 工具）；Args 非空则替换参数继续执行——替换后的参数不再二次校验，所以
// 钩子必须自己保证改写后的参数合法。
type ToolCallHookResult struct {
	Block     bool
	Reason    string
	Terminate bool
	// Mutated args (if the hook wants to rewrite them). They execute without
	// revalidation, as upstream executes a hook's mutated args.
	Args json.RawMessage
	// Err is the error the hook threw. Upstream's beforeToolCall throws instead (agent-loop.ts prepareToolCall catch): the tool does not run and its error result carries Err's message, even an empty one, with no default reason and no terminate.
	Err error
}

// BeforeToolCallHook is called before a tool executes.
// Return ToolCallHookResult{Block: true} to prevent execution.
type BeforeToolCallHook func(ctx context.Context, toolCallID, toolName string, args json.RawMessage) ToolCallHookResult

// AfterToolCallResult carries per-tool overrides returned by AfterToolCallHook.
// Mirrors upstream types.ts AfterToolCallResult (agent-loop.ts:617-640).
// Merge semantics: field-by-field replace; no deep merge. Nil fields keep the original; a non-nil empty Content replaces it with an empty array.
//
//   - Content: if non-nil, replaces the ordered content, including an empty array.
//   - IsError: if non-nil, replaces the tool result error flag.
//   - Usage: if non-nil, replaces the tool result usage.
//   - Terminate: if non-nil, replaces the early-termination hint. The agent
//     stops after the batch only when every finalized result terminates
//     (upstream shouldTerminateToolBatch).
type AfterToolCallResult struct {
	Content   []ai.ToolResultMessageContent
	Details   any
	IsError   *bool // nil = keep original
	Usage     *ai.Usage
	Terminate *bool
	// StructuredContent replaces the result's structured content. When Content is replaced without it, the structured content is dropped, because it may no longer match the content.
	StructuredContent json.RawMessage
}

// AfterToolCallHook is called after a tool executes and may override its result.
// Full upstream contract, including terminate signals and result overrides.
// Replaces the old void-return signature which had no override capability.
// upstream: agent-loop.ts afterToolCall({ toolCall, args, result, isError })
type AfterToolCallHook func(ctx context.Context, toolCallID, toolName string, args json.RawMessage, result AgentToolResult) AfterToolCallResult

// ─── Agent ────────────────────────────────────────────────────────────────────

// AgentInitialState seeds a new agent's state. A field that is set takes the place of the same-named AgentOptions field.
//
// 新 agent 的初始状态种子：填了的字段顶替 AgentOptions 同名字段
// （上游 AgentOptions.initialState，agent.ts:115）——恢复会话、克隆
// agent 时用它在构造期一次性灌入历史转录/模型/工具，而不是建完再改。
//
// upstream: packages/agent/src/agent.ts:115 (AgentOptions.initialState: Partial<Omit<AgentState, "pendingToolCalls" | "isStreaming" | ...>>)
type AgentInitialState struct {
	// SystemPrompt seeds the leading system message when Messages does not start with one.
	SystemPrompt  string
	Model         *ai.Model
	ThinkingLevel ai.ModelThinkingLevel
	Tools         []AgentTool
	// Messages is the starting transcript; it is copied.
	Messages []AgentMessage
}

// AgentOptions configures the agent loop.
//
// Agent 的全部可注入行为都集中在这个结构里，可分为四组：
//   - 模型与请求：Model / ThinkingLevel / ThinkingBudgets / Transport /
//     StreamFn / DefaultStreamFn / SessionID（prompt 缓存键）；
//   - 上下文变换：ConvertToLlm / TransformLLMMessages / PreparePrompt /
//     PrepareRequest / PrepareNextTurn / FinishTurn——Session 和扩展用
//     这些钩子在每次请求前后改写上下文、换模型、拦截收尾；
//   - 工具与权限：Tools / ToolExecution / BeforeToolCall / AfterToolCall /
//     PrepareToolResult；
//   - 事件与持久化：EventCh / EventDone / OnEvent / AfterEvent /
//     OnMessagePersist / OnProviderStreamEvent。
//
// 钩子在 runLoop 里的精确触发点见 agent_loop.go 的注释。
type AgentOptions struct {
	// InitialState seeds the model, thinking level, tools, system prompt and transcript.
	// upstream: agent.ts:115 (initialState)
	InitialState    *AgentInitialState
	Model           *ai.Model
	Tools           []AgentTool
	SystemPrompt    string
	MaxTurns        int // 0 = unlimited, as upstream, which has no turn cap
	ThinkingLevel   ai.ModelThinkingLevel
	ThinkingBudgets *ai.ThinkingBudgets
	Transport       ai.Transport
	// SessionID is forwarded to the provider for prompt caching
	// (OpenAI prompt_cache_key). Set by the session wrapper when
	// the session has a stable identifier.
	SessionID string
	// SessionFile, when set, reports the session file tools may expose as
	// PI_SESSION_FILE. Nil or an empty result exports nothing.
	SessionFile func() string

	// SteeringMode controls how steering messages are drained (default: one-at-a-time).
	// Mirrors upstream packages/agent/src/agent.ts:104.
	SteeringMode QueueMode
	// FollowUpMode controls how follow-up messages are drained (default: one-at-a-time).
	// Mirrors upstream packages/agent/src/agent.ts:105.
	FollowUpMode QueueMode

	// BeforeToolCall and AfterToolCall are Pi's single hook-function properties (agent.ts:201-207); BeforeToolCallHooks and AfterToolCallHooks are the Go hook lists, which run after them.
	BeforeToolCall      BeforeToolCallFunc
	AfterToolCall       AfterToolCallFunc
	BeforeToolCallHooks []BeforeToolCallHook
	AfterToolCallHooks  []AfterToolCallHook
	// PrepareToolResult processes the final extension-modified result before
	// events and persistence. Processing failures preserve the original result.
	PrepareToolResult func(context.Context, AgentToolResult) AgentToolResult
	// FinishTurn runs after a turn's assistant message and tool results and
	// before TurnEndEvent; it may end the run or request one more request.
	FinishTurn FinishTurn
	// PrepareRequest runs immediately before every provider request and may
	// replace the context, model, and thinking level for the run.
	PrepareRequest PrepareRequest
	// ConvertToLlm converts the transformed agent context to provider messages.
	// The run waits for it and reports conversion failures through its lifecycle.
	// Nil uses the standard message conversion.
	ConvertToLlm func([]AgentMessage) ([]ai.Message, error)
	// TransformLLMMessages post-processes the provider messages that the
	// built-in conversion produced for each request. The coding session uses
	// it as upstream createAgentSession wraps convertToLlm
	// (convertToLlmWithBlockImages in sdk.ts).
	TransformLLMMessages func([]ai.Message) []ai.Message
	// PrepareNextTurn is the hook that sees only the run's context. It is ignored when PrepareNextTurnWithContext is set.
	// upstream: agent.ts:127 (AgentOptions.prepareNextTurn(signal))
	PrepareNextTurn func(ctx context.Context) (*AgentLoopTurnUpdate, error)
	// PrepareNextTurnWithContext runs before the next turn starts when the loop
	// continues, receives the completed turn, and may replace or extend the next request's state.
	// upstream: agent.ts:130 (AgentOptions.prepareNextTurnWithContext(context, signal))
	PrepareNextTurnWithContext PrepareNextTurn
	// PreparePrompt lets the owning Session prepare new prompt messages before
	// their lifecycle events and persistence. It does not run for continuation
	// or queued steering/follow-up messages.
	PreparePrompt func(context.Context, []AgentMessage) ([]AgentMessage, error)
	// ToolExecution is the batch strategy for assistant messages with several
	// tool calls (default parallel). A tool whose ExecutionMode is sequential
	// forces its batch to run sequentially. Mirrors upstream Agent.toolExecution.
	ToolExecution ToolExecutionMode

	// EventCh receives agent events. If nil, events are discarded. Every
	// event is delivered, including after the run's context is cancelled, as
	// upstream awaits every listener; emit blocks until the event is received
	// or EventDone is closed.
	EventCh chan<- AgentEvent
	// EventDone, when closed, reports that EventCh has no receiver anymore.
	// Events emitted after that are dropped instead of blocking the agent.
	EventDone <-chan struct{}

	// OnEvent, if set, runs synchronously for every event before the event is
	// persisted or delivered, as upstream Agent awaits its listeners before it
	// continues. It may replace a message_end message in place (mutating the
	// pointed-to message or custom map), and the replacement is what agent
	// state, persistence and EventCh observe.
	OnEvent func(AgentEvent)
	// AfterEvent runs after persistence and channel publication, before the loop polls queued work. An error fails the same run.
	AfterEvent func(AgentEvent) error
	// StreamFn sends each provider request. Nil uses DefaultStreamFn or the configured host default; NewAgent fails when none exists.
	// Mirrors upstream AgentOptions.streamFn.
	StreamFn StreamFn
	// DefaultStreamFn supplies the host's stock request path without making it a caller override. Nil uses the configured host default.
	DefaultStreamFn StreamFn

	// OnMessagePersist, if set, is invoked exactly once for each NEW message
	// the agent produces during a turn: the user prompt, steering / follow-up
	// user messages, assistant messages (including aborted/error ones), and
	// tool-result messages: in production order. It is driven by message_end in
	// emit() and is NOT called for replayed/resumed context (the runLoop replay
	// emits message_end only for this run's new messages, never loaded history).
	// Mirrors upstream's single incremental persistence site (agent-session.ts
	// message_end handler) so a turn interrupted mid-flight is durably recorded
	// instead of lost on the next resume. An error fails the run the way a
	// throwing message_end listener does upstream: the loop stops and the run
	// ends with an error assistant message.
	OnMessagePersist func(AgentMessage) error
	// GetAPIKey resolves the credential for each provider request, for tokens that expire during a run. An empty result keeps the static key; an error fails the run, as a rejected getApiKey does. Mirrors upstream AgentOptions.getApiKey.
	GetAPIKey func(provider string) (string, error)
	// TransformContext rewrites the agent context before each provider request, ahead of ConvertToLlm. Mirrors upstream AgentOptions.transformContext; the context carries the run's AbortSignal and an error fails the turn.
	TransformContext func(context.Context, []AgentMessage) ([]AgentMessage, error)
	// OnPayload inspects or replaces each provider payload after the adapter builds it. SetBeforeProviderHook replaces it later. Mirrors upstream AgentOptions.onPayload.
	OnPayload func(payload any, model *ai.Model) (any, error)
	// OnResponse runs once per provider response before its body is consumed. Mirrors upstream AgentOptions.onResponse.
	OnResponse func(context.Context, ai.ProviderResponse, *ai.Model) error
	// MaxRetryDelayMs caps the delay a provider waits before one retry; nil keeps the provider default. Mirrors upstream AgentOptions.maxRetryDelayMs.
	MaxRetryDelayMs *int
	// Identity is how requests identify the app to providers, merged field by field over DefaultAgentIdentity (attribution to PiG). Pass an identity with AttributionNull to send no attribution headers. Mirrors upstream AgentOptions.identity.
	Identity *ai.ClientIdentity
	// OnProviderStreamEvent is forwarded to every provider request. Mirrors upstream AgentOptions.onProviderStreamEvent (packages/agent/src/agent.ts:122).
	OnProviderStreamEvent func(ctx context.Context, data any, model *ai.Model) error
}

// StreamFn starts one provider request for an already normalized transcript.
// Mirrors upstream StreamFn.
type StreamFn func(ctx context.Context, model *ai.Model, transcript ai.TranscriptContext, options ai.StreamOptions) (*ai.AssistantMessageEventStream, error)

// Agent is the agent loop.
//
// Mirrors upstream packages/agent/src/agent.ts (class Agent).
//
// Agent 是并发安全的有状态对象：同一时刻最多一个运行（streaming 标志 +
// beginRun/finishRun 互斥占用），但消息历史、工具列表、队列都可以在
// 运行中被其他 goroutine 读取/写入，因此各字段由不同锁保护：
//   - stateMu：运行状态（streaming、runContext、model/thinking 等运行时
//     可变选项）、listeners、stateRevision；
//   - messagesMu：messages 转录本体，保证运行中追加消息时外部仍能快照；
//   - toolsMu：opts.Tools 工具表；
//   - pendingNextTurnMu：下一回合注入消息。
//
// runContext/runCancel/runDone 构成一次运行的取消信号三角：Signal()
// 暴露给外界取消，Abort() 触发，WaitForIdle() 等在 runDone 上。
type Agent struct {
	// BeforeToolCall and AfterToolCall are Pi's public hook properties (agent.ts:201-207). They start as the options' functions and may be assigned between runs; a run reads them when it starts.
	BeforeToolCall BeforeToolCallFunc
	AfterToolCall  AfterToolCallFunc

	opts AgentOptions
	// validated remembers the history values every request of this agent has already validated.
	validated *ai.ValidationMemo
	// llm remembers the provider form of the history the previous request converted.
	llm                llmMemo
	stateMu            sync.RWMutex
	forcedSystemPrompt *string
	// loopTurns is the owner's model of Pi's event loop (jsloop.Turns), which the run's I/O and tools tell when the loop turns; nil for an owner that holds none.
	loopTurns *jsloop.Turns
	// messagesMu guards writes to messages and MessagesSnapshot reads, so
	// other goroutines can observe the transcript while a run appends to it.
	messagesMu sync.RWMutex
	// toolsMu guards opts.Tools: an extension registers a tool, and the Session replaces the tool list, while a run reads it.
	toolsMu       sync.RWMutex
	messages      []AgentMessage
	timings       *Recorder
	steeringQueue *PendingMessageQueue
	followUpQueue *PendingMessageQueue

	// pendingNextTurn holds messages an extension deferred with
	// deliverAs "nextTurn". Upstream keeps these apart from the steering and
	// follow-up queues because they are not continuations of a running turn:
	// they are context for the next user prompt, injected beside the user
	// message before the first model call (agent-session.ts:1226).
	pendingNextTurn   []AgentMessage
	pendingNextTurnMu sync.Mutex
	streaming         bool // true while Send/Continue is actively streaming
	runContext        context.Context
	runCancel         context.CancelFunc
	runSignalWatchers []*runSignalWatcher
	stopParentCancel  func() bool
	runDone           chan struct{}
	listeners         []*agentListener
	streamingMessage  *AgentMessage
	// streamingStorage holds the message streamingMessage points at while it streams, so a message_update stores it without allocating a copy.
	streamingStorage AgentMessage
	// streamingCapture is the partial of the last message_update, captured instead of delivered because nothing observed it; streamingDeferred says streamingStorage is still to be built from it (StreamingMessage).
	streamingCapture  ai.ShallowCapture
	streamingDeferred bool
	pendingToolCalls  []string
	errorMessage      string

	// beforeProviderHook transforms the provider's final wire payload.
	// Mirrors upstream before_provider_request via StreamOptions.OnPayload.
	beforeProviderHook func(payload any, model *ai.Model) (any, error)

	// transformHeaders runs on each provider request's merged HTTP headers.
	// Mirrors upstream buildRequestOptions.transformHeaders (sdk.ts).
	transformHeaders func(context.Context, ai.ProviderHeaders) (ai.ProviderHeaders, error)

	// transformContext fires before each LLM call to let extensions
	// modify the message context. Mirrors upstream context event via
	// runner.emitContext → transformContext in sdk.ts.
	transformContext func(context.Context, []AgentMessage) ([]AgentMessage, error)
}

// NewAgent creates a new Agent.
//
// 构造时只做三件事：克隆工具表（防外部修改）、填充缺省值（无模型时给
// unknown 占位、thinking 默认 off、队列模式默认 one-at-a-time、工具执行
// 默认并行）、把系统提示写成转录的第一条 system 消息（附工具声明）。
// 不会发起任何请求——真正的循环要等 Send/Continue。
// NewAgent creates a new Agent. Like Pi's constructor (`streamFn ?? getDefaultStreamFn()`), it fails with ErrNoDefaultStreamFunction when
// the options carry no stream function and no host default is configured; a model's own Provider does not stand in for one.
func NewAgent(opts AgentOptions) (*Agent, error) {
	var seeded []AgentMessage
	if initial := opts.InitialState; initial != nil {
		if initial.Model != nil {
			opts.Model = initial.Model
		}
		if initial.ThinkingLevel != "" {
			opts.ThinkingLevel = initial.ThinkingLevel
		}
		if initial.Tools != nil {
			opts.Tools = initial.Tools
		}
		if initial.SystemPrompt != "" {
			opts.SystemPrompt = initial.SystemPrompt
		}
		seeded = slices.Clone(initial.Messages)
	}
	opts.Tools = slices.Clone(opts.Tools)
	if opts.DefaultStreamFn == nil {
		configured, err := GetDefaultStreamFn()
		if err != nil && opts.StreamFn == nil {
			return nil, err
		}
		opts.DefaultStreamFn = configured
	}
	if opts.Model == nil {
		// DEFAULT_MODEL has an explicit empty input list; adapters must not infer text support for it.
		opts.Model = &ai.Model{ID: "unknown", DisplayName: "unknown", Input: []string{}, ProviderMeta: ai.ProviderMetadata{ProviderID: "unknown", API: "unknown"}}
	}
	if opts.ThinkingLevel == "" {
		opts.ThinkingLevel = ai.ThinkingOff
	}
	sm := opts.SteeringMode
	if sm == "" {
		sm = QueueModeOneAtATime
	}
	fm := opts.FollowUpMode
	if fm == "" {
		fm = QueueModeOneAtATime
	}
	if opts.ToolExecution == "" {
		opts.ToolExecution = ToolModeParallel
	}
	a := &Agent{
		BeforeToolCall: opts.BeforeToolCall,
		AfterToolCall:  opts.AfterToolCall,
		opts:           opts,
		validated:      ai.NewValidationMemo(),
		timings:        NewRecorder(),
		steeringQueue:  NewPendingMessageQueue(sm),
		followUpQueue:  NewPendingMessageQueue(fm),

		beforeProviderHook: opts.OnPayload,
		transformContext:   opts.TransformContext,
	}
	a.messages = seeded
	// upstream: agent.ts:84-86 (a system message is prepended unless the transcript already starts with one)
	if len(a.messages) == 0 || a.messages[0].System == nil {
		if initial := ai.CreateInitialSystemMessage(opts.SystemPrompt, toolDeclarations(a.toolList())); initial != nil {
			a.messages = append([]AgentMessage{{System: initial}}, a.messages...)
		}
	}
	return a, nil
}

// Timings returns the agent's timing recorder. Read-only consumers
// (status line, /cost, --diagnose) hold this reference for the
// session's lifetime and call Snapshot()/Elapsed() on demand.
func (a *Agent) Timings() *Recorder { return a.timings }

// Tools returns the agent's current tool list. Defensive copy.
func (a *Agent) Tools() []AgentTool {
	return a.toolList()
}

// SetTools replaces the active tool list for subsequent turns.
func (a *Agent) SetTools(tools []AgentTool) {
	a.toolsMu.Lock()
	defer a.toolsMu.Unlock()
	a.opts.Tools = append([]AgentTool(nil), tools...)
}

// toolList is a copy of the current tool list.
func (a *Agent) toolList() []AgentTool {
	a.toolsMu.RLock()
	defer a.toolsMu.RUnlock()
	return slices.Clone(a.opts.Tools)
}

// SetSystemPrompt projects a replacement system prompt onto subsequent provider requests without rewriting transcript history.
func (a *Agent) SetSystemPrompt(prompt string) {
	a.messagesMu.Lock()
	defer a.messagesMu.Unlock()
	a.opts.SystemPrompt = prompt
	a.forcedSystemPrompt = new(prompt)
}

// ClearSystemPrompt removes a SetSystemPrompt projection, so later requests
// use the transcript's own system prompt again.
func (a *Agent) ClearSystemPrompt() {
	a.messagesMu.Lock()
	defer a.messagesMu.Unlock()
	a.forcedSystemPrompt = nil
}

// SystemPromptOverride returns only the explicit provider projection and its presence, not replayed transcript instructions.
func (a *Agent) SystemPromptOverride() (string, bool) {
	a.messagesMu.RLock()
	defer a.messagesMu.RUnlock()
	if a.forcedSystemPrompt == nil {
		return "", false
	}
	return *a.forcedSystemPrompt, true
}

func (a *Agent) systemPromptOverride() *string {
	a.messagesMu.RLock()
	defer a.messagesMu.RUnlock()
	return a.forcedSystemPrompt
}

// ErrNoModelSelected is returned when a turn is requested with no usable model.
// Upstream agent-session.prompt() throws formatNoModelSelectedMessage() before
// streaming; pig surfaces the same condition as an error rather than panicking
// on a nil model in runLoop.
var ErrNoModelSelected = errors.New("no model selected")

// ErrAlreadyProcessingPrompt is returned when a prompt is sent while a run is
// active. Mirrors upstream Agent.prompt's "Agent is already processing a
// prompt" error: queue the message with Steer or FollowUp instead.
var ErrAlreadyProcessingPrompt = errors.New("Agent is already processing a prompt. Use steer() or followUp() to queue messages, or wait for completion.")

// ErrAlreadyProcessing is returned when Continue or Reset is called while a
// run is active. Mirrors upstream Agent.continue and Agent.reset.
var ErrAlreadyProcessing = errors.New("Agent is already processing.")

// ErrNoMessagesToContinue is returned by Continue on an empty transcript.
// Mirrors upstream Agent.continue's "No messages to continue from".
var ErrNoMessagesToContinue = errors.New("No messages to continue from")

// ErrToolResultsUnanswered reports that a run ended with tool results the
// model never answered although no tool asked to terminate the run. It guards
// the invariant that a run never ends silently mid-task.
var ErrToolResultsUnanswered = errors.New("agent run ended after tool results without a model response")

// ErrMaxTurnsReached reports that a run stopped at AgentOptions.MaxTurns with
// tool results still waiting for a model response.
var ErrMaxTurnsReached = errors.New("agent stopped at its turn limit with tool results still unanswered")

// ensureModel requires a model and either an injected stream function or a provider runtime.
// 每次进入 runLoop 前的门卫：没有可用模型、也没有任何发请求的途径
// （调用方注入的 StreamFn、宿主默认 DefaultStreamFn、或模型自带
// Provider）时直接拒绝，防止在循环深处才 nil 解引用。
// ensureModel requires a model and a stream function.
func (a *Agent) ensureModel() error {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	if a.opts.Model == nil {
		return ErrNoModelSelected
	}
	if a.opts.StreamFn == nil && a.opts.DefaultStreamFn == nil {
		return ErrNoDefaultStreamFunction
	}
	return nil
}

// beginRun claims the agent for one run, or returns busy when a run is active.
// Mirrors upstream Agent's activeRun guard.
//
// 运行互斥的入口：streaming=true 期间任何新 Send/Continue 都立即失败，
// 调用方应改用 Steer/FollowUp 排队。成功占用后会派生本次运行的
// runDeadlineContext：它从父 ctx 继承 deadline、转发父级取消，但用
// context.WithoutCancel 切断"父 ctx 结束 ⇒ 运行 ctx 必然结束"的硬绑定，
// 使运行结束时还能保住未消费的取消语义（Abort 的语义等价于上游 abort
// signal）。
func (a *Agent) beginRun(ctx context.Context, busy error) error {
	if err := a.claimRun(ctx, busy); err != nil {
		return err
	}
	a.notifyRunSignalObservers()
	return nil
}

func (a *Agent) claimRun(ctx context.Context, busy error) error {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if a.streaming {
		return busy
	}
	// Detach the parent's cancellation registration at settlement without
	// aborting a successfully completed run's retained signal.
	// WithoutCancel 切断父子取消硬绑定；deadline 单独继承；父取消改由
	// AfterFunc 显式转发，运行结束时能摘掉这次注册。
	runContext, cancel := context.WithCancel(jsloop.WithTurns(context.WithoutCancel(ctx), a.loopTurns))
	deadline, hasDeadline := ctx.Deadline()
	signal := &runDeadlineContext{Context: ai.WithStreamContinuations(runContext), parent: ctx, deadline: deadline, hasDeadline: hasDeadline}
	signal.active.Store(true)
	a.runContext = signal
	a.runCancel = cancel
	a.stopParentCancel = context.AfterFunc(ctx, cancel)
	if ctx.Err() != nil {
		cancel()
	}
	a.runDone = make(chan struct{})
	a.streaming = true
	a.streamingMessage = nil
	a.dropStreamingCapture()
	return nil
}

// finishRun releases the claim beginRun took.
func (a *Agent) finishRun() {
	a.releaseRun()
	a.notifyRunSignalObservers()
}

func (a *Agent) releaseRun() {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.stopParentCancel()
	signal := a.runContext.(*runDeadlineContext)
	// 父 ctx 若在运行期间已经取消，补一次 cancel 把该状态固化进运行
	// ctx——否则 active=false 后 Err() 只剩内层，外部看到的中断会丢。
	if signal.parent.Err() != nil {
		a.runCancel()
	}
	signal.active.Store(false)
	a.streaming = false
	a.streamingMessage = nil
	a.dropStreamingCapture()
	a.pendingToolCalls = nil
	a.runContext = nil
	a.runCancel = nil
	a.stopParentCancel = nil
	close(a.runDone)
	a.runDone = nil
	// 运行结束统一清扫惰性注销的监听器（运行中不打标记即时删，避免
	// notifyListeners 遍历到一半切片被改）。
	a.listeners = slices.DeleteFunc(a.listeners, func(listener *agentListener) bool { return listener.removed.Load() })
}

// Send sends a plain-text user message and runs the agent loop until the LLM
// stops calling tools. Steering and follow-up messages queued via
// Steer/FollowUp are polled at the appropriate points in the loop.
//
// 最常用的入口：阻塞到整个运行结束，返回本次运行新增的消息。
// 完整链路：Send → SendContent → BeginSendContent（占用运行权、校验模型）
// → PromptRun.Start（准备 prompt、发 agent_start）→ runLoop（agent_loop.go）。
// 想边运行边观察事件，应订阅事件或读 EventCh，而不是轮询返回值。
func (a *Agent) Send(ctx context.Context, content string) ([]AgentMessage, error) {
	return a.SendContent(ctx, []ai.UserContentBlock{ai.TextContent{Text: content}})
}

// SendContent is the structured-content variant of Send.
func (a *Agent) SendContent(ctx context.Context, content []ai.UserContentBlock) ([]AgentMessage, error) {
	run, err := a.BeginSendContent(ctx, content)
	if err != nil {
		return nil, err
	}
	return run.Run()
}

// PromptRun owns a claimed prompt and its cancellation context. Start dispatches agent_start; Run drains the remaining events and releases the claim. The owner must call Run exactly once, even if Start fails or the context is cancelled.
//
// 把"一次 prompt 运行"拆成两步的结构：BeginSendContent/BeginSendMessages
// 先占住运行权并返回 PromptRun；Start() 做 prompt 准备并发出 agent_start；
// Run() 才真正进入 runLoop 并在返回时释放运行权（defer finishRun）。
// 拆分的意义：占用运行权和发出第一个事件之间有一段窗口，调用方可以在
// Start 之前先做自己的事（比如先注册监听器），又不会让别人插进来。
// 约定：无论 Start 成败，持有方必须恰好调用一次 Run()，否则运行权泄漏。
type PromptRun struct {
	agent            *Agent
	ctx              context.Context
	content          []ai.UserContentBlock
	messages         []AgentMessage
	suppliedMessages bool
	prompts          []AgentMessage
	started          bool
	prepared         bool
	// failed is set when a listener failure ended the run while agent_start was dispatched; the run is over, as in Pi, where the failure unwinds runAgentLoop before turn_start.
	failed   bool
	startErr error
}

// Start prepares the prompt and dispatches agent_start without requesting a Provider response or committing unfinished prompt messages. Repeated calls return the first result.
func (r *PromptRun) Start() error {
	if r.started {
		return r.startErr
	}
	r.started = true
	a := r.agent
	var messages []AgentMessage
	if r.suppliedMessages {
		// 调用方直接给了成品消息（SendMessages）：只过 PreparePrompt 钩子。
		messages = r.messages
		if prepare := a.PreparePromptHook(); prepare != nil {
			messages, r.startErr = prepare(r.ctx, messages)
		}
	} else {
		// 常规路径：文本/块内容 → user 消息 + 排队的 nextTurn 消息，
		// 再过 PreparePrompt（prepareContent 内部完成）。
		messages, r.startErr = a.prepareContent(r.ctx, r.content)
	}
	if r.startErr != nil {
		return r.startErr
	}
	// 写入工具集差分声明：扩展在两次运行间增删了工具时，模型必须看到
	// toolsAdded/toolsRemoved 变化声明，否则它的工具视图和实际不符。
	tools := a.toolList()
	a.messagesMu.RLock()
	r.prompts = declareToolChanges(tools, a.messages, messages)
	a.messagesMu.RUnlock()
	r.prepared = true
	a.stateMu.Lock()
	a.errorMessage = ""
	a.stateMu.Unlock()
	// agent_start 是第一个事件；监听器在 Start 阶段抛错同样走失败收尾，
	// 保证事件序列不残缺。
	err, failure := catchRunFailure(func() error { a.emit(AgentStartEvent{}); return nil })
	if failure != nil {
		r.failed = true
		err = a.handleRunFailure(failure, r.ctx.Err() != nil, a.Model())
	}
	r.startErr = err
	return err
}

// Run drains the claimed prompt and releases its active-run state, including when Start failed. The owner calls Run once; it calls Start when needed.
func (r *PromptRun) Run() ([]AgentMessage, error) {
	defer r.agent.finishRun()
	if err := r.Start(); err != nil {
		if r.prepared {
			return r.agent.messages, err
		}
		return nil, err
	}
	if r.failed {
		return r.agent.messages, nil
	}
	cfg := r.agent.createLoopConfig(false)
	cfg.agentStarted = true
	return r.agent.runLoop(r.ctx, cfg, r.prompts)
}

// BeginSendContent claims the agent and binds its cancellation context synchronously without starting events. Pending preflight is not an active run; a claimed prompt is active before its first awaited listener finishes.
func (a *Agent) BeginSendContent(ctx context.Context, content []ai.UserContentBlock) (*PromptRun, error) {
	if err := a.beginRun(ctx, ErrAlreadyProcessingPrompt); err != nil {
		return nil, err
	}
	if err := a.ensureModel(); err != nil {
		a.finishRun()
		return nil, err
	}
	return &PromptRun{agent: a, ctx: a.Signal(), content: slices.Clone(content)}, nil
}

// BeginSendMessages claims a turn for already-built messages without starting events. The owner calls Run exactly once, even if Start fails, just as for BeginSendContent.
func (a *Agent) BeginSendMessages(ctx context.Context, messages []AgentMessage) (*PromptRun, error) {
	if err := a.beginRun(ctx, ErrAlreadyProcessingPrompt); err != nil {
		return nil, err
	}
	if err := a.ensureModel(); err != nil {
		a.finishRun()
		return nil, err
	}
	return &PromptRun{agent: a, ctx: a.Signal(), messages: messages, suppliedMessages: true}, nil
}

func (a *Agent) prepareContent(ctx context.Context, content []ai.UserContentBlock) ([]AgentMessage, error) {
	userMsg := AgentMessage{
		User: &UserMessage{
			Role:      RoleUser,
			Content:   append(ai.UserContentBlocks(nil), content...),
			Timestamp: time.Now().UnixMilli(),
		},
	}
	// user 消息在前、队列里积攒的 nextTurn 上下文消息在后：扩展在上个
	// 回合要求"下一回合带给模型"的消息借此注入到首个请求之前。
	msgs := append([]AgentMessage{userMsg}, a.takePendingNextTurn()...)
	if prepare := a.PreparePromptHook(); prepare != nil {
		var err error
		msgs, err = prepare(ctx, msgs)
		if err != nil {
			return nil, err
		}
	}
	return msgs, nil
}

// SendMessages seeds a turn with already-built messages instead of a user
// prompt, mirroring upstream agent.prompt(messages) (agent-session.ts:1063).
// An extension delivering a custom message while the agent is idle needs a turn
// to start from that message; without this the message can only be enqueued for
// a turn that may never run.
func (a *Agent) SendMessages(ctx context.Context, msgs []AgentMessage) ([]AgentMessage, error) {
	if len(msgs) == 0 {
		return a.messages, nil
	}
	run, err := a.BeginSendMessages(ctx, msgs)
	if err != nil {
		return nil, err
	}
	return run.Run()
}

// ContinueMessages runs the agent loop from the current message state without
// prepending a new user message and returns the messages the run produced.
// Used to retry a turn after auto-compaction when an overflow error is
// recovered by compacting and replaying. Continue is the same run in Pi's shape.
//
// If the last message is an assistant message, ContinueMessages first checks the
// steering queue, then the follow-up queue. Without queued input it rejects
// the assistant tail. Mirrors upstream Agent.continue (packages/agent/src/agent.ts).
//
// 适用场景：压缩/重写历史后续跑、从中断点恢复。与 Send 的区别是不注入
// 新 user 消息，直接在现有转录尾部继续。尾部是 assistant 消息时没有
// 合法续接点（模型不能接自己的话），除非队列里有转向/跟进消息把它顶掉。
func (a *Agent) ContinueMessages(ctx context.Context) ([]AgentMessage, error) {
	if err := a.beginRun(ctx, fmt.Errorf("%w Wait for completion before continuing.", ErrAlreadyProcessing)); err != nil {
		return nil, err
	}
	defer a.finishRun()
	ctx = a.Signal()
	lastMsg := a.lastMessage()
	// 空转录、或整条转录只有 system 消息（没有任何对话内容）都没法续。
	if lastMsg == nil || slices.IndexFunc(a.messages, func(m AgentMessage) bool { return m.System == nil }) == -1 {
		return a.messages, ErrNoMessagesToContinue
	}
	if lastMsg.Assistant != nil {
		// Upstream: drain steering first, then follow-ups.
		// 尾部是 assistant 时不能直接请求（模型不能接自己的话）；但队列里
		// 若有用户排的消息，可把它们当本次运行的新 prompt 续接。注意
		// skipInitialSteeringPoll=true：steering 已经在这里被取走，循环
		// 不能再重复取一次。
		if steered := a.steeringQueue.Drain(); len(steered) > 0 {
			return a.runPromptMessages(ctx, steered, a.createLoopConfig(true))
		}
		if followUps := a.followUpQueue.Drain(); len(followUps) > 0 {
			return a.runPromptMessages(ctx, followUps, a.createLoopConfig(false))
		}
		return a.messages, fmt.Errorf("Cannot continue from message role: assistant")
	}
	return a.runAgentLoopContinue(ctx, a.createLoopConfig(false))
}

// runPromptMessages prepares a prompt batch and runs its lifecycle before the
// first provider request. Mirrors upstream Agent.runPromptMessages.
// 队列取出的消息（steering/follow-up 批次）作为本次运行的 prompt 进入
// 循环：先过 declareToolChanges 补工具差分，再交给 runLoop。
func (a *Agent) runPromptMessages(ctx context.Context, msgs []AgentMessage, cfg AgentLoopConfig) ([]AgentMessage, error) {
	return a.runLoop(ctx, cfg, declareToolChanges(a.toolList(), a.messages, msgs))
}

func (a *Agent) lastMessage() *AgentMessage {
	if len(a.messages) == 0 {
		return nil
	}
	return &a.messages[len(a.messages)-1]
}

// Messages returns the current message history.
func (a *Agent) Messages() []AgentMessage { return a.messages }

// MessagesSnapshot copies the message history. Unlike Messages it is safe to
// call from any goroutine while a run appends.
func (a *Agent) MessagesSnapshot() []AgentMessage {
	a.messagesMu.RLock()
	defer a.messagesMu.RUnlock()
	return slices.Clone(a.messages)
}

// setMessages replaces the message history under messagesMu.
func (a *Agent) setMessages(msgs []AgentMessage) {
	a.messagesMu.Lock()
	a.messages = msgs
	a.messagesMu.Unlock()
}

// appendMessages extends the message history under messagesMu.
func (a *Agent) appendMessages(msgs ...AgentMessage) {
	a.messagesMu.Lock()
	a.messages = append(a.messages, msgs...)
	a.messagesMu.Unlock()
}

// SystemPrompt returns the current replayed instructions or an explicit provider prompt override. It is safe to call while the agent appends messages.
func (a *Agent) SystemPrompt() string {
	prompt, _ := a.SystemPromptSnapshot()
	return prompt
}

// SystemPromptSnapshot reads the prompt and whether it is present under one lock. Presence distinguishes an empty projected prompt from an uninitialized transcript.
func (a *Agent) SystemPromptSnapshot() (string, bool) {
	a.messagesMu.RLock()
	defer a.messagesMu.RUnlock()
	if a.forcedSystemPrompt != nil {
		return *a.forcedSystemPrompt, true
	}
	systems := systemMessages(a.messages)
	return ai.GetCurrentSystemPrompt(systems), len(systems) > 0
}

// SetMessages replaces the message history (used for session restore). The
// agent keeps a copy of the top-level slice, as upstream's state.messages
// setter copies the assigned array.
// 会话恢复/历史重写的入口：整个替换转录（克隆防外部再改）。
// 与 appendMessages 的区别：这是全量换底，不产生消息生命周期事件。
func (a *Agent) SetMessages(msgs []AgentMessage) { a.setMessages(slices.Clone(msgs)) }

// SetSessionID updates the session ID used for prompt caching.
// Called after session creation/resume when the stable session ID is known.
func (a *Agent) SetSessionID(id string) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.opts.SessionID = id
}

// SetModel swaps the active LLM model. The next run uses the new
// provider/model; a run in flight keeps the model it started with (agent.ts:471). The agent's tools, system prompt, and
// message history are unchanged. Mid-session model switch.
func (a *Agent) SetModel(m *ai.Model) {
	a.stateMu.Lock()
	a.opts.Model = m
	a.stateMu.Unlock()
}

// SetBeforeProviderHook transforms each provider's final wire payload. It is the assignment of Pi's public Agent.onPayload (agent.ts:198):
// a run captures the hook when it starts (createLoopConfig, agent.ts:473), so an assignment during a run takes effect on the next run.
func (a *Agent) SetBeforeProviderHook(fn func(payload any, model *ai.Model) (any, error)) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.beforeProviderHook = fn
}

// Ports packages/agent/src/agent.ts
// ─── 可变运行选项的读写器 ───
//
// 下面这一组 Get*/Set*/Hook() 方法对应上游 Agent 上可被外界直接读写
// 的公开字段/钩子（agent.ts：Session 与扩展会在运行间隙替换
// agent.convertToLlm、agent.prepareRequest 等）。共同语义：
//   - 全部经 stateMu 串行化；
//   - 运行开始时由 createLoopConfig 快照一次——运行中改值只影响下一次
//     运行，不回溯当前请求；
//   - *Hook() 形式读出当前值，供调用方链式包装后再 Set 回去。

// StreamFunction returns the caller's stream override. Nil denotes the stock stream, allowing summarization to distinguish optional custom-stream auth from required stock auth without comparing Go function values.
func (a *Agent) StreamFunction() StreamFn {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.opts.StreamFn
}

// EffectiveStreamFunction returns the function every run sends provider requests through: the caller's override, else the default stream. Mirrors upstream Agent.streamFunction (`runtimeOptions.streamFn ?? getDefaultStreamFn()`), which summarization passes to compact and generateBranchSummary so those requests take the same request path as the agent's. Unlike [Agent.StreamFunction], it is never the nil that stands for the stock stream.
func (a *Agent) EffectiveStreamFunction() StreamFn {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	if a.opts.StreamFn != nil {
		return a.opts.StreamFn
	}
	return a.opts.DefaultStreamFn
}

// GetAPIKeyFunc returns the credential resolver the next run uses, or nil. Mirrors upstream Agent.getApiKey.
func (a *Agent) GetAPIKeyFunc() func(provider string) (string, error) {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.opts.GetAPIKey
}

// SetGetAPIKey replaces the credential resolver for subsequent runs; nil removes it. Mirrors assigning upstream Agent.getApiKey, which each run reads when it starts.
func (a *Agent) SetGetAPIKey(fn func(provider string) (string, error)) {
	a.stateMu.Lock()
	a.opts.GetAPIKey = fn
	a.stateMu.Unlock()
}

// SetStreamFunction replaces the provider request function for subsequent runs: a run reads it once when it starts (agent.ts:443,455), matching Agent.streamFunction assignment in Pi. Nil restores the stock stream.
func (a *Agent) SetStreamFunction(streamFn StreamFn) {
	a.stateMu.Lock()
	a.opts.StreamFn = streamFn
	a.stateMu.Unlock()
}

// SetTransformHeaders sets the hook that rewrites each provider request's
// merged HTTP headers. Mirrors upstream StreamOptions.transformHeaders.
func (a *Agent) SetTransformHeaders(fn func(context.Context, ai.ProviderHeaders) (ai.ProviderHeaders, error)) {
	a.stateMu.Lock()
	a.transformHeaders = fn
	a.stateMu.Unlock()
}

// SetTransformContext sets a hook that fires before each LLM call to allow
// extensions to modify the message context. Mirrors assigning upstream Agent.transformContext (agent.ts:494): a run reads it once when it starts, so an assignment during a run applies to the next one.
func (a *Agent) SetTransformContext(fn func(msgs []AgentMessage) []AgentMessage) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if fn == nil {
		a.transformContext = nil
		return
	}
	a.transformContext = func(_ context.Context, messages []AgentMessage) ([]AgentMessage, error) {
		return fn(messages), nil
	}
}

// SetTransformContextWithContext installs a request transform with cancellation
// and error propagation owned by the active agent run.
func (a *Agent) SetTransformContextWithContext(fn func(context.Context, []AgentMessage) ([]AgentMessage, error)) {
	a.stateMu.Lock()
	a.transformContext = fn
	a.stateMu.Unlock()
}

// Model returns the active model. New agents use the upstream unknown
// descriptor until one is selected; SetModel(nil) clears it.
func (a *Agent) Model() *ai.Model {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.opts.Model
}

// ThinkingLevel returns the reasoning depth used for future Send calls.
func (a *Agent) ThinkingLevel() ai.ModelThinkingLevel {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.opts.ThinkingLevel
}

// SetThinkingLevel updates the reasoning depth used for future Send calls.
// Wired by InteractiveMode.cycleThinkingLevel() on Shift+Tab.
// The current in-flight call (if any) is unaffected; the new level applies
// starting with the next agent.Send invocation.
func (a *Agent) SetThinkingLevel(level ai.ModelThinkingLevel) {
	a.stateMu.Lock()
	a.opts.ThinkingLevel = level
	a.stateMu.Unlock()
}

// SetTransport updates the preferred provider transport for future Send calls.
func (a *Agent) SetTransport(transport ai.Transport) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.opts.Transport = transport
}

// Transport returns the preferred provider transport. Mirrors upstream Agent.transport (agent.ts:187).
func (a *Agent) Transport() ai.Transport {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.opts.Transport
}

// SessionID returns the session identifier forwarded to providers. Mirrors upstream Agent.sessionId (agent.ts:183).
func (a *Agent) SessionID() string {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.opts.SessionID
}

// ThinkingBudgets returns the per-level thinking token budgets forwarded to the stream function, or nil. Mirrors upstream Agent.thinkingBudgets (agent.ts:185).
func (a *Agent) ThinkingBudgets() *ai.ThinkingBudgets {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.opts.ThinkingBudgets
}

// SetThinkingBudgets replaces the per-level thinking token budgets for future runs.
func (a *Agent) SetThinkingBudgets(budgets *ai.ThinkingBudgets) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.opts.ThinkingBudgets = budgets
}

// MaxRetryDelayMs returns the cap on provider-requested retry delays, or nil. Mirrors upstream Agent.maxRetryDelayMs (agent.ts:189).
func (a *Agent) MaxRetryDelayMs() *int {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.opts.MaxRetryDelayMs
}

// SetMaxRetryDelayMs replaces the cap on provider-requested retry delays for future runs; nil keeps each provider's default.
func (a *Agent) SetMaxRetryDelayMs(maxRetryDelayMs *int) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.opts.MaxRetryDelayMs = maxRetryDelayMs
}

// Identity returns the request identity merged over DefaultAgentIdentity, or nil. Mirrors upstream Agent.identity.
func (a *Agent) Identity() *ai.ClientIdentity {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.opts.Identity
}

// SetIdentity replaces the request identity for future runs; nil leaves DefaultAgentIdentity alone.
func (a *Agent) SetIdentity(identity *ai.ClientIdentity) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.opts.Identity = identity
}

// SetToolExecution replaces the batch strategy for assistant messages with several tool calls, for future runs. Mirrors assigning upstream Agent.toolExecution (agent.ts:191).
func (a *Agent) SetToolExecution(mode ToolExecutionMode) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.opts.ToolExecution = mode
}

// ConvertToLlm returns the converter from agent messages to provider messages, or nil for the standard conversion. Mirrors upstream Agent.convertToLlm (agent.ts:155).
func (a *Agent) ConvertToLlm() func([]AgentMessage) ([]ai.Message, error) {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.opts.ConvertToLlm
}

// SetConvertToLlm replaces the converter from agent messages to provider messages for future runs; nil restores the standard conversion.
func (a *Agent) SetConvertToLlm(convert func([]AgentMessage) ([]ai.Message, error)) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.opts.ConvertToLlm = convert
}

// OnResponse returns the hook that runs once per provider response before its body is consumed. Mirrors upstream Agent.onResponse (agent.ts:160).
func (a *Agent) OnResponse() func(context.Context, ai.ProviderResponse, *ai.Model) error {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.opts.OnResponse
}

// SetOnResponse replaces the provider response hook for future runs.
func (a *Agent) SetOnResponse(hook func(context.Context, ai.ProviderResponse, *ai.Model) error) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.opts.OnResponse = hook
}

// OnProviderStreamEvent returns the observer forwarded to every provider request. Mirrors upstream Agent.onProviderStreamEvent (agent.ts:161).
func (a *Agent) OnProviderStreamEvent() func(ctx context.Context, data any, model *ai.Model) error {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.opts.OnProviderStreamEvent
}

// SetOnProviderStreamEvent replaces the provider stream event observer for future runs.
func (a *Agent) SetOnProviderStreamEvent(observe func(ctx context.Context, data any, model *ai.Model) error) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.opts.OnProviderStreamEvent = observe
}

// IsStreaming returns true when the agent is actively in a Send/Continue loop.
// Mirrors upstream AgentState.isStreaming for extension queries.
func (a *Agent) IsStreaming() bool {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.streaming
}

// ─── Steering / Follow-up queues ──────────────────────────────────────────────
// Mirrors upstream packages/agent/src/agent.ts:252-279.

// Steer enqueues a steering message. Steering messages are delivered
// after the current tool batch finishes but before the next LLM call,
// allowing the user to redirect the agent mid-turn.
//
// 转向消息：运行进行中用户插话用的。它会在当前工具批次执行完、下一次
// 模型请求之前被取走并注入上下文（runLoop 内层循环的 getSteeringMessages
// 轮询点），从而在本回合内改变模型接下来看到的内容。
func (a *Agent) Steer(msg AgentMessage) { a.steeringQueue.Enqueue(msg) }

// FollowUp enqueues a follow-up message. Follow-up messages are delivered
// only after the agent has no more tool calls AND no steering messages,
// starting a fresh outer-loop iteration.
//
// 跟进消息：等当前回复彻底结束（没有更多工具调用、也没有转向消息）
// 后才作为新一轮输入进入外层循环——相当于"自动再发一条 user 消息"，
// 而不是打断当前回合。
func (a *Agent) FollowUp(msg AgentMessage) { a.followUpQueue.Enqueue(msg) }

// QueueNextTurn defers a message to the start of the next user turn, where it
// is injected beside the user message as context for the first model call.
//
// This is not FollowUp. A follow-up continues the turn that is running, so it
// reaches the model only after the current response completes; a message meant
// to inform the next prompt would arrive a model call too late, and on an idle
// agent it would sit in a queue with no goroutine to drain it.
func (a *Agent) QueueNextTurn(msg AgentMessage) {
	a.pendingNextTurnMu.Lock()
	defer a.pendingNextTurnMu.Unlock()
	a.pendingNextTurn = append(a.pendingNextTurn, msg)
}

// TakePendingNextTurn removes and returns the messages QueueNextTurn deferred. A run that starts from built messages delivers them beside its starting message (agent-session.ts:1918-1920 _prepareRun).
//
// 取走并清空扩展经 QueueNextTurn 攒下的"下一轮上下文"消息：从成品
// 消息起跑的运行把它们排在起始消息之后一起发给模型。
func (a *Agent) TakePendingNextTurn() []AgentMessage { return a.takePendingNextTurn() }

// takePendingNextTurn removes and returns the deferred messages.
func (a *Agent) takePendingNextTurn() []AgentMessage {
	a.pendingNextTurnMu.Lock()
	defer a.pendingNextTurnMu.Unlock()
	msgs := a.pendingNextTurn
	a.pendingNextTurn = nil
	return msgs
}

// ClearSteeringQueue removes all queued steering messages. Mirrors upstream agent.ts clearSteeringQueue, which returns nothing.
func (a *Agent) ClearSteeringQueue() { a.steeringQueue.Clear() }

// ClearFollowUpQueue removes all queued follow-up messages. Mirrors upstream agent.ts clearFollowUpQueue, which returns nothing.
func (a *Agent) ClearFollowUpQueue() { a.followUpQueue.Clear() }

// HasQueuedMessages reports whether either queue still contains pending
// messages. Mirrors upstream agent.ts:290.
func (a *Agent) HasQueuedMessages() bool {
	return a.steeringQueue.HasItems() || a.followUpQueue.HasItems()
}

// ClearAllQueues removes all queued steering and follow-up messages. Mirrors upstream agent.ts clearAllQueues, which returns nothing.
func (a *Agent) ClearAllQueues() {
	a.ClearSteeringQueue()
	a.ClearFollowUpQueue()
}

// ClearQueuedUserMessages removes the queued user messages from both queues and leaves queued custom messages in place.
// pig divergence (D135): upstream clearAllQueues also destroys custom messages that nothing reports back to the caller.
//
// 只清两条队列里的 user 消息，custom 消息原样保留——D135 与上游
// clearAllQueues 的分歧点：上游会连同无人认领的 custom 一起销毁。
func (a *Agent) ClearQueuedUserMessages() {
	a.steeringQueue.ClearUserMessages()
	a.followUpQueue.ClearUserMessages()
}

// PeekQueuedMessages previews the messages selected for the next turn without
// consuming them: the steering queue's selection, or the follow-up queue's
// when no steering is queued. Mirrors upstream Agent.peekQueuedMessages.
func (a *Agent) PeekQueuedMessages() []AgentMessage {
	if steering := a.steeringQueue.Peek(); len(steering) > 0 {
		return steering
	}
	return a.followUpQueue.Peek()
}

// HasPendingMessages reports whether either queue has messages.
func (a *Agent) HasPendingMessages() bool {
	return a.steeringQueue.HasItems() || a.followUpQueue.HasItems()
}

// PendingMessages returns a snapshot of both queues without draining.
// Used for UI display (e.g. "2 steering, 1 follow-up pending").
func (a *Agent) PendingMessages() (steering, followUp []AgentMessage) {
	return a.steeringQueue.Messages(), a.followUpQueue.Messages()
}

// SetSteeringMode changes the steering queue's drain policy.
func (a *Agent) SetSteeringMode(mode QueueMode) { a.steeringQueue.SetMode(mode) }

// SetFollowUpMode changes the follow-up queue's drain policy.
func (a *Agent) SetFollowUpMode(mode QueueMode) { a.followUpQueue.SetMode(mode) }

// SteeringMode returns the current steering drain policy.
func (a *Agent) SteeringMode() QueueMode { return a.steeringQueue.Mode() }

// FollowUpMode returns the current follow-up drain policy.
func (a *Agent) FollowUpMode() QueueMode { return a.followUpQueue.Mode() }

// Reset retains the replayed system baseline and clears conversation state and queues. It refuses while a run is
// active, as upstream Agent.reset throws. Mirrors upstream Agent.reset.
func (a *Agent) Reset() error {
	a.stateMu.RLock()
	streaming := a.streaming
	a.stateMu.RUnlock()
	if streaming {
		return fmt.Errorf("%w Wait for completion before resetting.", ErrAlreadyProcessing)
	}
	// Reset 保留"当前生效的系统基线"（系统提示+工具声明所在的那条
	// system 消息），其余对话历史和队列全部清掉——新一轮对话从干净的
	// 指令开始而不是从零开始。
	baseline := ai.GetCurrentSystemMessage(a.messages)
	var messages []AgentMessage
	if baseline != nil {
		messages = []AgentMessage{{System: baseline}}
	}
	a.setMessages(messages)
	a.stateMu.Lock()
	a.streamingMessage = nil
	a.dropStreamingCapture()
	a.pendingToolCalls = nil
	a.errorMessage = ""
	a.stateMu.Unlock()
	a.steeringQueue.Clear()
	a.followUpQueue.Clear()
	return nil
}

// SetPreparePrompt replaces the hook that prepares new prompt messages before a run starts, for later runs. It is the Go counterpart of the prompt preparation an owning Session installs (agent-session.ts _installAgentForcedPromptProjection); PreparePromptHook returns the current one so a caller can chain it.
func (a *Agent) SetPreparePrompt(fn func(context.Context, []AgentMessage) ([]AgentMessage, error)) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.opts.PreparePrompt = fn
}

// PreparePromptHook returns the current prompt-preparation hook, or nil.
func (a *Agent) PreparePromptHook() func(context.Context, []AgentMessage) ([]AgentMessage, error) {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.opts.PreparePrompt
}

// SetPrepareToolResult replaces the hook that processes each final extension-modified tool result, for later runs.
func (a *Agent) SetPrepareToolResult(fn func(context.Context, AgentToolResult) AgentToolResult) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.opts.PrepareToolResult = fn
}

// PrepareToolResultHook returns the current tool-result hook, or nil.
func (a *Agent) PrepareToolResultHook() func(context.Context, AgentToolResult) AgentToolResult {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.opts.PrepareToolResult
}

// SetTransformLLMMessages replaces the hook that post-processes the provider messages before each request, for later runs. TransformLLMMessagesHook returns the current one so a caller can chain it, as agent-session.ts _installHiddenDeclarationsProjection chains transformContext.
func (a *Agent) SetTransformLLMMessages(fn func([]ai.Message) []ai.Message) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.opts.TransformLLMMessages = fn
}

// TransformLLMMessagesHook returns the current provider-message hook, or nil.
func (a *Agent) TransformLLMMessagesHook() func([]ai.Message) []ai.Message {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.opts.TransformLLMMessages
}

// SetSessionFile replaces the function that reports the session file tools may expose, for later runs.
func (a *Agent) SetSessionFile(fn func() string) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.opts.SessionFile = fn
}

// SetFinishTurn replaces the FinishTurn hook for later runs. Mirrors assigning
// upstream's public Agent.finishTurn; FinishTurnHook returns the current one
// so a caller can chain it.
func (a *Agent) SetFinishTurn(fn FinishTurn) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.opts.FinishTurn = fn
}

// FinishTurnHook returns the current FinishTurn hook, or nil.
func (a *Agent) FinishTurnHook() FinishTurn {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.opts.FinishTurn
}

// SetPrepareRequest replaces the PrepareRequest hook for later runs. Mirrors
// assigning upstream's public Agent.prepareRequest.
func (a *Agent) SetPrepareRequest(fn PrepareRequest) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.opts.PrepareRequest = fn
}

// PrepareRequestHook returns the current PrepareRequest hook, or nil.
func (a *Agent) PrepareRequestHook() PrepareRequest {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.opts.PrepareRequest
}

// SetPrepareNextTurnWithContext replaces the PrepareNextTurnWithContext hook for later runs. Mirrors
// assigning upstream's public Agent.prepareNextTurnWithContext.
func (a *Agent) SetPrepareNextTurnWithContext(fn PrepareNextTurn) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.opts.PrepareNextTurnWithContext = fn
}

// PrepareNextTurnWithContextHook returns the current PrepareNextTurnWithContext hook, or nil.
func (a *Agent) PrepareNextTurnWithContextHook() PrepareNextTurn {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.opts.PrepareNextTurnWithContext
}

// SetPrepareNextTurn replaces the context-only PrepareNextTurn hook for later runs. Mirrors assigning upstream's public Agent.prepareNextTurn.
func (a *Agent) SetPrepareNextTurn(fn func(ctx context.Context) (*AgentLoopTurnUpdate, error)) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.opts.PrepareNextTurn = fn
}

// PrepareNextTurnHook returns the current context-only PrepareNextTurn hook, or nil.
func (a *Agent) PrepareNextTurnHook() func(ctx context.Context) (*AgentLoopTurnUpdate, error) {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.opts.PrepareNextTurn
}

// nextTurnHook is the hook the loop runs: the one that receives the turn wins, as upstream's `prepareNextTurnWithContext || prepareNextTurn`.
// The caller holds stateMu.
//
// 两版 PrepareNextTurn 钩子的取舍：带 turn 上下文的新版优先；旧式无
// context 版包一层适配器丢弃上下文参数（上游
// `prepareNextTurnWithContext || prepareNextTurn` 的回退顺序）。
func (a *Agent) nextTurnHook() PrepareNextTurn {
	if a.opts.PrepareNextTurnWithContext != nil {
		return a.opts.PrepareNextTurnWithContext
	}
	if legacy := a.opts.PrepareNextTurn; legacy != nil {
		return func(ctx context.Context, _ PrepareNextTurnContext) (*AgentLoopTurnUpdate, error) { return legacy(ctx) }
	}
	return nil
}

// ToolExecutionMode returns the batch strategy for assistant messages with several tool calls. Mirrors upstream Agent.toolExecution.
func (a *Agent) ToolExecutionMode() ToolExecutionMode {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.opts.ToolExecution
}

// AddBeforeToolCallHook appends a hook that fires before each tool execution.
// Mirrors upstream agent.beforeToolCall assignment (agent-session.ts:373).
func (a *Agent) AddBeforeToolCallHook(h BeforeToolCallHook) {
	a.stateMu.Lock()
	a.opts.BeforeToolCallHooks = append(a.opts.BeforeToolCallHooks, h)
	a.stateMu.Unlock()
}

// AddAfterToolCallHook appends a hook that fires after each tool execution.
// Mirrors upstream agent.afterToolCall assignment (agent-session.ts:396).
func (a *Agent) AddAfterToolCallHook(h AfterToolCallHook) {
	a.stateMu.Lock()
	a.opts.AfterToolCallHooks = append(a.opts.AfterToolCallHooks, h)
	a.stateMu.Unlock()
}

// persistMessage invokes the OnMessagePersist hook for a newly produced
// message. Driven by message_end in emit(), mirroring upstream's single
// persistence site (agent-session.ts:511-525). System, user, assistant, toolResult and
// custom messages persist here; bash-execution entries keep their own append
// path.
//
// Custom messages are included because an extension can queue one with
// deliverAs steer/followUp while a turn is in flight. Persisting at creation
// instead would record it between an assistant's tool_use and its tool_result,
// which replays as an invalid sequence ("`tool_use` ids were found without
// `tool_result` blocks immediately after").
//
// 持久化的唯一入口：只在 message_end 触发、只对本次运行新产生的消息
// 触发（恢复的历史不重放），保证"中断的回合也有落盘记录"且不会写重。
func (a *Agent) persistMessage(msg AgentMessage) error {
	if a.opts.OnMessagePersist == nil {
		return nil
	}
	if msg.System == nil && msg.User == nil && msg.Assistant == nil && msg.ToolResult == nil && msg.Custom == nil {
		return nil
	}
	return a.opts.OnMessagePersist(msg)
}

// runFailure unwinds the agent loop when handling an event fails, as a
// throwing listener rejects upstream Agent.processEvents and ends the run.
//
// 运行级失败的信号载体。监听器/持久化/钩子出错时用 panic(runFailure)
// 一路弹出到 runLoop 边界，由 catchRunFailure 捕获并转成正常的错误返回。
// 用 panic 而非逐层 error 传递，是因为失败点（emit 深处）和恢复点
// （runLoop 顶层）之间隔着多层不返回 error 的调用；其他不相关 panic
// 会原样再抛出，不被误吞。
type runFailure struct{ err error }

// catchRunFailure runs fn on the loop goroutine and returns the handler
// failure that unwound it, if any. Other panics propagate.
//
// 运行边界处的"解 panic"闸门：只接 runFailure，其余 panic 继续传播。
// 返回值区分两种结果：fn 自身的 error（正常错误路径）与 failure
// （被 panic 中断的处理器失败）。
func catchRunFailure(fn func() error) (runErr, failure error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			raised, ok := recovered.(runFailure)
			if !ok {
				panic(recovered)
			}
			failure = raised.err
		}
	}()
	return fn(), nil
}

// handleRunFailure ends a run that a failing event handler interrupted with an
// error assistant message, turn_end, and agent_end, and returns the failure
// that interrupted delivering them. Mirrors upstream Agent.handleRunFailure.
//
// 运行失败的收尾协议：即使是监听器/持久化出错导致的中断，也要补发一条
// error/aborted 的 assistant 消息和 turn_end、agent_end，让外界看到
// 一个完整闭合的事件序列（TUI 不挂起、持久化有落点、会话层能分类错误）。
// 收尾过程中再次失败则返回新的 failure。
func (a *Agent) handleRunFailure(err error, aborted bool, model *ai.Model) error {
	stopReason := ai.StopReasonError
	if aborted {
		stopReason = ai.StopReasonAborted
	}
	message := &AssistantMessage{
		Role:         RoleAssistant,
		Content:      []ai.AssistantContentBlock{ai.TextContent{Text: ""}},
		Usage:        &ai.Usage{},
		StopReason:   stopReason,
		ErrorMessage: err.Error(),
		Timestamp:    time.Now().UnixMilli(),
	}
	if model != nil {
		message.API = model.ProviderMeta.API
		message.ModelID = model.ID
		message.Provider = model.ProviderMeta.ProviderID
		if model.Provider != nil {
			message.Provider = model.Provider.ID()
		}
	}
	failureMessage := AgentMessage{Assistant: message}
	// 补发完整的失败回合事件序列：message_start/end + turn_end +
	// agent_end。这些事件自身也可能因监听器失败而中断——所以同样套
	// catchRunFailure，第二层失败作为返回值上抛。
	_, failure := catchRunFailure(func() error {
		a.emit(MessageStartEvent{Message: AgentMessage{Assistant: cloneAssistantMessage(message)}})
		a.emit(MessageEndEvent{Message: failureMessage})
		a.emit(TurnEndEvent{Message: failureMessage})
		a.emit(AgentEndEvent{Messages: []AgentMessage{failureMessage}})
		return nil
	})
	return failure
}

// ─── Internal helpers ─────────────────────────────────────────────────────────

// emit 是全部事件的唯一出口，处理顺序固定且每一环都有语义：
//  1. message_end 先把消息提交进公开转录（appendMessages）——监听器
//     看到 message_end 时消息必须已经在 Messages() 里；
//  2. reduceEventState 更新 streamingMessage / pendingToolCalls /
//     errorMessage 等派生状态（lifecycle.go）；
//  3. notifyListeners 同步调用所有 Subscribe 监听器——监听器返回错误
//     会以 runFailure panic 终止运行（对应上游 await 监听器抛异常）；
//  4. OnEvent 同步钩子，可原地替换 message_end 的消息；
//  5. persistMessage 对每条完成消息恰好持久化一次（OnMessagePersist）；
//  6. 推送到 EventCh（有 EventDone 兜底防阻塞：接收方退出后丢事件而
//     不是卡死 agent）；
//  7. AfterEvent 后置钩子，出错同样终止运行。
//
//perf:hot
func (a *Agent) emit(ev AgentEvent) {
	// message_end commits to public state before listeners run. The provider
	// context is separate, so unfinished or future prompt messages stay hidden.
	if end, ok := ev.(MessageEndEvent); ok {
		a.appendMessages(end.Message)
	}
	a.reduceEventState(ev)
	a.notifyListeners(ev)
	if a.opts.OnEvent != nil {
		a.opts.OnEvent(ev)
	}
	// Persist each completed message once. Prompt, steering and follow-up messages emit message_end when admitted; resumed history does not replay its lifecycle.
	var persistErr error
	if me, ok := ev.(MessageEndEvent); ok {
		persistErr = a.persistMessage(me.Message)
	}
	if a.opts.EventCh != nil {
		select {
		case a.opts.EventCh <- ev:
		case <-a.opts.EventDone:
		}
	}
	if persistErr != nil {
		panic(runFailure{err: persistErr})
	}
	if a.opts.AfterEvent != nil {
		if err := a.opts.AfterEvent(ev); err != nil {
			panic(runFailure{err: err})
		}
	}
}

// toolCallArguments is intentionally a byte slice rather than strings.Builder.
// pendingToolCall values are copied while a batch is prepared and finalized;
// copying a non-zero strings.Builder is invalid and its copyCheck panics when
// a later caller appends to the copied value.
type toolCallArguments []byte

func (a *toolCallArguments) Write(p []byte) (int, error) {
	*a = append(*a, p...)
	return len(p), nil
}

func (a *toolCallArguments) WriteString(s string) (int, error) {
	*a = append(*a, s...)
	return len(s), nil
}

func (a toolCallArguments) String() string { return string(a) }

type pendingToolCall struct {
	id               string
	name             string
	args             toolCallArguments
	thoughtSignature string // encrypted reasoning context for replay
}

// consumeStream drives one provider response into agent events. thinking is the level the request asked for; it is recorded on the final response, whichever stream function answered (upstream agent-loop.ts:408-409). Partial messages do not carry it.
//
// 把一个 provider 事件流"泵"成 agent 层事件并返回最终 assistant 消息。
// 事件映射：start→message_start，各类 delta→message_update，
// done→message_end 并返回消息+提取出的工具调用列表，
// error→message_end（返回 nil 工具调用）。
// 两条非典型路径：流从未 start 就 done/error（合成 start 再收尾）；
// ctx 被取消或流非正常耗尽（把已有消息标记 aborted 并收尾）。
// 工具调用不在此执行——返回值交给 runTurn 决定是否继续。
func (h *loopHost) consumeStream(ctx context.Context, stream *ai.AssistantMessageEventStream, model *ai.Model, thinking ai.ModelThinkingLevel) (*AssistantMessage, []pendingToolCall, error) {
	thinking = recordedThinkingLevel(thinking)
	// result 闭包：把 provider 的最终消息转成 agent 层消息并盖上"本次
	// 请求要求的 thinking 级别"——无论实际由哪个 streamFn 应答。
	result := func(final *ai.AssistantMessage) *AssistantMessage {
		message := agentAssistantMessage(final)
		message.ThinkingLevel = thinking
		return message
	}
	var message *AssistantMessage
	started := false
	// unobserved is the partial of the last message_update nothing observed; pending says message is still to be built from it.
	var unobserved ai.ShallowCapture
	pending := false

	caller := ai.StreamObservationFromContext(ctx)
	iteratorContext := ctx
	if signal, ok := ctx.(*runDeadlineContext); ok {
		// Agent.abort waits for the provider's terminal response, preserving its
		// error and draining its owned work. The Go caller can still cancel its
		// own context if it must stop waiting for a non-responsive provider.
		// 迭代改用父 ctx：Abort 取消的是运行 ctx，但这里要继续把 provider
		// 的终结事件（done/error）收完，拿不到终结事件就无法给出带正确
		// 错误信息的定稿消息。
		iteratorContext = signal.parent
	}
	// start 之前的杂项事件直接跳过；第一个有效事件必须是
	// start/done/error 三者之一才有资格建立 message。
	// Events arrive unrefreshed: each is refreshed at the tick it arrives, unless it is a message_update that nothing can observe, whose delivery is skipped whole (h.skipUnobservedUpdate).
	for observation, event := range stream.ObservePublishedEvents(caller.Context(iteratorContext)) {
		if !started {
			switch event.(type) {
			case ai.StartEvent, ai.DoneEvent, ai.ErrorEvent:
			default:
				continue
			}
		}
		if partial, update := assistantUpdatePartial(event); update && !h.messageUpdateObserved() {
			unobserved.Capture(partial)
			pending = true
			h.skipUnobservedUpdate(&unobserved)
			continue
		}
		pending = false
		event = ai.RefreshEvent(event)
		switch event := event.(type) {
		case ai.StartEvent:
			// 流开始：建浅视图并发出 message_start。
			message = shallowAssistantMessage(event.Partial)
			started = true
			h.emitObserved(observation, MessageStartEvent{Message: AgentMessage{Assistant: message}})
		case ai.TextStartEvent:
			message = h.emitAssistantUpdate(observation, event.Partial, event)
		case ai.TextDeltaEvent:
			message = h.emitAssistantUpdate(observation, event.Partial, event)
		case ai.TextEndEvent:
			message = h.emitAssistantUpdate(observation, event.Partial, event)
		case ai.ThinkingStartEvent:
			message = h.emitAssistantUpdate(observation, event.Partial, event)
		case ai.ThinkingDeltaEvent:
			message = h.emitAssistantUpdate(observation, event.Partial, event)
		case ai.ThinkingEndEvent:
			message = h.emitAssistantUpdate(observation, event.Partial, event)
		case ai.ToolCallStartEvent:
			message = h.emitAssistantUpdate(observation, event.Partial, event)
		case ai.ToolCallDeltaEvent:
			message = h.emitAssistantUpdate(observation, event.Partial, event)
		case ai.ToolCallEndEvent:
			message = h.emitAssistantUpdate(observation, event.Partial, event)
		case ai.DoneEvent:
			// 正常终结：取最终消息；若流从未 start 过（provider 一次性
			// 返回），先补发 message_start 再补 message_end，保证事件
			// 序列完整闭合。
			message = result(stream.Result())
			awaitResultWrapper(observation)
			if !started {
				h.emitObserved(observation, MessageStartEvent{Message: AgentMessage{Assistant: cloneAssistantMessage(message)}})
			}
			message = h.endAssistantMessage(observation, message)
			return message, pendingToolCalls(message), nil
		case ai.ErrorEvent:
			// provider 侧错误终结：同样补全生命周期后返回；error 本身
			// 已通过 message.ErrorMessage 携带，返回 nil error。
			message = result(stream.Result())
			awaitResultWrapper(observation)
			if !started {
				h.emitObserved(observation, MessageStartEvent{Message: AgentMessage{Assistant: cloneAssistantMessage(message)}})
			}
			return h.endAssistantMessage(observation, message), nil, nil
		}
	}

	// 事件流耗尽但没见到 done/error：只能被动收尾（常见于取消）。
	final, resultErr := stream.ResultContext(ctx)
	if ctx.Err() != nil || resultErr != nil {
		if pending {
			message = shallowAssistantMessageFrom(&unobserved)
		}
		if message == nil {
			// 连 start 都没收到：凭空合成一条 aborted 消息，provider/
			// model 元信息尽量填上，供错误分类。
			providerID, modelID := "", ""
			if model != nil {
				modelID = model.ID
				if model.Provider != nil {
					providerID = model.Provider.ID()
				}
			}
			message = result(&ai.AssistantMessage{
				Provider: providerID, Model: modelID,
				StopReason: ai.StopReasonAborted, Timestamp: time.Now().UnixMilli(),
			})
		} else {
			// 收到过部分内容：定稿快照改标 aborted。
			message = message.Observe()
			message.StopReason = ai.StopReasonAborted
			message.ThinkingLevel = thinking
		}
		if !started {
			h.emit(MessageStartEvent{Message: AgentMessage{Assistant: cloneAssistantMessage(message)}})
		}
		return h.endAssistantMessage(nil, message), nil, ctx.Err()
	}

	// 流正常耗尽但没有 done（少见）：同样补全生命周期，按已有 final 收尾。
	finished := result(final)
	if !started {
		h.emit(MessageStartEvent{Message: AgentMessage{Assistant: cloneAssistantMessage(finished)}})
	}
	finished = h.endAssistantMessage(nil, finished)
	return finished, pendingToolCalls(finished), nil
}

// awaitResultWrapper is the reaction that upstream's `result` wrapper adds to `await response.result()` (packages/agent/src/agent-loop.ts:409 `const result = async () => Object.assign(await response.result(), ...)`, awaited at :445 and :460): the wrapper's own promise settles one reaction after the inner await.
func awaitResultWrapper(observation *ai.StreamObservation) {
	if observation != nil {
		observation.Yield()
	}
}

// recordedThinkingLevel is the thinkingLevel a final response records for a request that asked for level (upstream agent-loop.ts:409, `config.reasoning ?? "off"`).
func recordedThinkingLevel(level ai.ModelThinkingLevel) ai.ModelThinkingLevel {
	if level == "" {
		return ai.ThinkingOff
	}
	return level
}

// endAssistantMessage emits message_end for a finalized assistant response and
// returns the message the transcript records. The event and the transcript
// share it, as upstream shares one object, so an OnEvent replacement applied
// in place reaches agent state, persistence and listeners alike.
// 定稿拷贝一份再发 message_end：emit 里这条消息会进公开转录并
// 持久化，之后不能再被流式视图带动变化。
func (h *loopHost) endAssistantMessage(observation *ai.StreamObservation, message *AssistantMessage) *AssistantMessage {
	final := cloneAssistantMessage(message)
	h.emitObserved(observation, MessageEndEvent{Message: AgentMessage{Assistant: final}})
	return final
}

// emitAssistantUpdate 是所有非终结流事件的统一出口：按 partial 当前
// 状态建浅视图，包成 message_update 发出。浅视图让 TUI 看到事件发出
// 瞬间的顶层字段，同时嵌套内容仍跟随流推进。
func (h *loopHost) emitAssistantUpdate(observation *ai.StreamObservation, partial *ai.AssistantMessage, event ai.AssistantMessageEvent) *AssistantMessage {
	message := shallowAssistantMessage(partial)
	h.emitObserved(observation, MessageUpdateEvent{
		Message:               AgentMessage{Assistant: message},
		AssistantMessageEvent: event,
	})
	return message
}

// upstream: packages/agent/src/agent-loop.ts:416-453 awaits the event sink even when its synchronous prefix completes without blocking.
//
// emitObserved = emit + 一次 Yield：把流观察域挂到事件上发出后，让渡
// 给观察域中排队的反应（对应上游 await 事件总线的微任务边界），保证
// 同步派发路径和异步路径的回调顺序一致。
func (h *loopHost) emitObserved(observation *ai.StreamObservation, event AgentEvent) {
	h.emit(WithEventObservation(event, observation))
	if observation != nil {
		observation.Yield()
	}
}

// messageUpdateObserved reports whether anything could see a message_update now. A run without a gate (a standalone run, whose sink is the caller's) is always observed.
//
// 此刻发 message_update 是否有观察者：无闸门的运行（独立 run，sink
// 就是调用方）恒为可见；Session 侧装了闸门时由闸门判定。
func (h *loopHost) messageUpdateObserved() bool {
	return h.messageUpdatesObserved == nil || h.messageUpdatesObserved()
}

// skipUnobservedUpdate stands for emitAssistantUpdate when nothing can observe the update: no event, no listener, no await. The Agent's state still records the update (Agent.reduceUnobservedUpdate).
//
// 无观察者时替代 emitAssistantUpdate 的旁路：不发事件、不唤醒监听器、
// 不等 await，但仍把这次更新记账进 Agent 状态（reduceUnobservedUpdate）——
// 省掉每次 delta 都要走一遍的同步派发成本。
//
// Upstream awaits its listeners for every update (agent-loop.ts:432-439, agent.ts processEvents, agent-session.ts _handleAgentEvent); none of those reactions does anything here, and the only other continuation that can run between them, the provider's, never waits for this consumer, so where its reactions fall between the consumer's changes nothing either can observe (docs/parity/equivalence/lw-quiet-b02c2ceabc.md).
func (h *loopHost) skipUnobservedUpdate(capture *ai.ShallowCapture) {
	if h.unobservedUpdate != nil {
		h.unobservedUpdate(capture)
	}
}

// assistantUpdatePartial returns the partial of an event the Agent emits as message_update, and whether it is one.
//
// 判定 provider 事件是否会被转译成 message_update（TextXxx /
// ThinkingXxx / ToolCallXxx 九种），是则同时取出其中的 partial。
func assistantUpdatePartial(event ai.AssistantMessageEvent) (*ai.AssistantMessage, bool) {
	switch event := event.(type) {
	case ai.TextStartEvent:
		return event.Partial, true
	case ai.TextDeltaEvent:
		return event.Partial, true
	case ai.TextEndEvent:
		return event.Partial, true
	case ai.ThinkingStartEvent:
		return event.Partial, true
	case ai.ThinkingDeltaEvent:
		return event.Partial, true
	case ai.ThinkingEndEvent:
		return event.Partial, true
	case ai.ToolCallStartEvent:
		return event.Partial, true
	case ai.ToolCallDeltaEvent:
		return event.Partial, true
	case ai.ToolCallEndEvent:
		return event.Partial, true
	}
	return nil, false
}

// upstream: packages/agent/src/agent-loop.ts:408-433
//
// shallowAssistantMessage 把 provider 的流式部分消息包成 agent 层
// AssistantMessage：拷贝顶层字段，但保留 streamView 指回 provider 内部
// 对象——这样顶层值冻结在事件发出的瞬间，而嵌套内容（Content 等）仍随
// 流推进，Observe() 读取时才取最新快照。
// The shallow copy is the one snapshot of the partial at this tick: for a live view its exported fields are an owned copy of the captured publication, so the message takes them instead of observing the copy again. The view reads the stream through its retained references, never through those fields. A plain partial's shallow copy shares the producer's nested values, so the message observes (deep-copies) it.
func shallowAssistantMessage(partial *ai.AssistantMessage) *AssistantMessage {
	var capture ai.ShallowCapture
	capture.Capture(partial)
	return shallowAssistantMessageFrom(&capture)
}

// shallowAssistantMessageFrom is shallowAssistantMessage for the partial as capture recorded it, at whatever later time it is called. The message records the captured publication, so a refresh keeps it while the stream has published nothing since.
//
// 延迟版浅视图构造：按 capture 当时记录的发布快照建消息——之后流若
// 没再发布新内容，refresh 看到的仍是这份快照，不会回退成更旧的视图。
func shallowAssistantMessageFrom(capture *ai.ShallowCapture) *AssistantMessage {
	view := capture.ShallowCopy()
	cell, seq := capture.Publication()
	var message *AssistantMessage
	if cell != 0 {
		message = observedAgentAssistantMessage(view)
	} else {
		message = observedAgentAssistantMessage(view.Observe())
	}
	message.streamView = view
	message.streamSeq = seq
	return message
}

// agentAssistantMessage 把 provider 层的 ai.AssistantMessage 拍平成
// agent 层 AssistantMessage：逐字段拷贝顶层值（不共享嵌套切片），
// 顺手把所有思维块文本汇总进 Thinking 便利字段。
func agentAssistantMessage(message *ai.AssistantMessage) *AssistantMessage {
	if message == nil {
		return &AssistantMessage{Role: RoleAssistant}
	}
	usage := message.Usage
	return assistantMessageWith(message, append([]ai.AssistantContentBlock(nil), message.Content...), append([]ai.AssistantMessageDiagnostic(nil), message.Diagnostics...), &usage)
}

// observedAgentAssistantMessage is agentAssistantMessage for an observation the caller owns and drops (ai.AssistantMessage.Observe's result): the agent message takes its content, diagnostics and usage instead of copying them. An empty slice becomes nil, as agentAssistantMessage's copy does.
//
// 免拷贝版转换：调用方已 Observe 出快照且不再保留 observation 时，
// 直接接管其 content/diagnostics/usage 切片（空切片归一成 nil，
// 与拷贝版行为一致）。
func observedAgentAssistantMessage(observed *ai.AssistantMessage) *AssistantMessage {
	if observed == nil {
		return &AssistantMessage{Role: RoleAssistant}
	}
	content, diagnostics := observed.Content, observed.Diagnostics
	if len(content) == 0 {
		content = nil
	}
	if len(diagnostics) == 0 {
		diagnostics = nil
	}
	return assistantMessageWith(observed, content, diagnostics, &observed.Usage)
}

// assistantMessageWith 是上面几个转换变体的公共构造：按调用方定好的
// content/diagnostics/usage 组装 agent 层 AssistantMessage，其余顶层
// 字段从 provider 消息平移。
func assistantMessageWith(message *ai.AssistantMessage, content []ai.AssistantContentBlock, diagnostics []ai.AssistantMessageDiagnostic, usage *ai.Usage) *AssistantMessage {
	out := &AssistantMessage{
		Role:                  RoleAssistant,
		Content:               content,
		Timestamp:             message.Timestamp,
		Usage:                 usage,
		API:                   message.API,
		Provider:              message.Provider,
		ModelID:               message.Model,
		ResponseModel:         message.ResponseModel,
		ResponseID:            message.ResponseID,
		ProviderThinkingLevel: message.ProviderThinkingLevel,
		ThinkingLevel:         message.ThinkingLevel,
		Diagnostics:           diagnostics,
		Deferred:              message.Deferred,
		StopReason:            message.StopReason,
		ErrorMessage:          message.ErrorMessage,
		ErrorBeforeTimestamp:  message.ErrorBeforeTimestamp,
		RawStopReason:         message.RawStopReason,
		EndTurn:               message.EndTurn,
		DurationMs:            message.DurationMs,
	}
	for _, block := range content {
		if thinking, ok := block.(ai.ThinkingContent); ok {
			out.Thinking += thinking.Thinking
			out.ThinkingSignature = thinking.ThinkingSignature
		}
	}
	return out
}

func newPendingToolCall(call ai.ToolCall) pendingToolCall {
	arguments, err := call.ArgumentsJSON()
	if err != nil {
		arguments = []byte("{}")
	}
	pending := pendingToolCall{id: call.ID, name: call.Name, thoughtSignature: call.ThoughtSignature}
	// ArgumentsJSON returns bytes the call owns; the pending call keeps them, clipped so an append by a reader of the shared bytes copies.
	pending.args = slices.Clip(arguments)
	return pending
}

// pendingToolCalls 从定稿的 assistant 消息里抽出全部 tool_call 块，
// 转成待执行队列（参数取原始 JSON 字节，取不到用 "{}" 兜底）。
// 这是 consumeStream → runTurn 之间的交接点。
func pendingToolCalls(message *AssistantMessage) []pendingToolCall {
	calls := make([]pendingToolCall, 0)
	for _, block := range message.Content {
		call, ok := block.(ai.ToolCall)
		if !ok {
			continue
		}
		pending := newPendingToolCall(call)
		calls = append(calls, pending)
	}
	return calls
}

// cloneAssistantMessage copies a finished or synthesized message for a lifecycle event. Its producer no longer mutates it, so an owned copy reads as Pi's object spread does.
func cloneAssistantMessage(msg *AssistantMessage) *AssistantMessage {
	if msg == nil {
		return nil
	}
	msg = msg.Observe()
	cp := *msg
	cp.Content = append([]ai.AssistantContentBlock(nil), msg.Content...)
	if msg.EndTurn != nil {
		cp.EndTurn = new(*msg.EndTurn)
	}
	if msg.DurationMs != nil {
		cp.DurationMs = new(*msg.DurationMs)
	}
	return &cp
}
