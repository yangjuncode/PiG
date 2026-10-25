// tool_call_hook_context.go 定义传给工具调用钩子的上下文载荷：
// 发起调用的 assistant 消息 + 当时的 agent 上下文（消息与工具表），
// 经 context.Context 透传给 before/after 钩子。

package agent

import (
	"context"
	"slices"
)

// ToolCallHookContext is what Pi hands a tool-call hook beside the call itself: the assistant message that issued the call and the agent context at that point.
//
// A before or after hook receives it through its context.Context ([ToolCallHookContextFrom]); the loop and [RunToolCall] attach it before the hooks run.
//
// upstream: packages/agent/src/types.ts BeforeToolCallContext / AfterToolCallContext (assistantMessage, context)
type ToolCallHookContext struct {
	// AssistantMessage issued the tool call. Nil for a RunToolCall made without one.
	AssistantMessage *AssistantMessage
	// Context is the agent context (messages and tools) the call runs in.
	Context AgentContext
}

type toolCallHookContextKey struct{}

// WithToolCallHookContext returns ctx carrying the hook context.
func WithToolCallHookContext(ctx context.Context, hookContext ToolCallHookContext) context.Context {
	return context.WithValue(ctx, toolCallHookContextKey{}, hookContext)
}

// ToolCallHookContextFrom returns the hook context a tool-call hook runs with.
func ToolCallHookContextFrom(ctx context.Context) (ToolCallHookContext, bool) {
	hookContext, ok := ctx.Value(toolCallHookContextKey{}).(ToolCallHookContext)
	return hookContext, ok
}

// toolCallHookContext snapshots the loop's state for the hooks of one assistant message's calls.
func (r *loopRun) toolCallHookContext(assistant *AssistantMessage) ToolCallHookContext {
	return ToolCallHookContext{AssistantMessage: assistant, Context: AgentContext{Messages: slices.Clone(r.context), Tools: r.h.tools()}}
}
