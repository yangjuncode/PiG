// agent_prompt.go 是上游 agent.prompt / agent.continue 的入口方法：
// Prompt（文本+图片）、PromptMessages（成品消息批）、Continue（续跑）。
// 与 Send/SendMessages 的区别在返回值：上游这几个方法 resolve 为空，
// 所以这里也只报 error，产出的消息由 Send 变体返回。

package agent

import (
	"context"

	"github.com/MichaelKinsy/PiG/ai"
)

// Prompt is Pi's agent.prompt(input, images?) (agent.ts:372): it starts a run from a user message made of the text and the images and returns when the run settles. Pi's method resolves to nothing; the produced messages are the transcript's new tail, which Send returns to a caller that wants them.
//
// Like Pi, it fails when a run is already active.
func (a *Agent) Prompt(ctx context.Context, input string, images ...ai.ImageContent) error {
	content := make([]ai.UserContentBlock, 0, 1+len(images))
	content = append(content, ai.TextContent{Text: input})
	for _, image := range images {
		content = append(content, image)
	}
	_, err := a.SendContent(ctx, content)
	return err
}

// PromptMessages is Pi's agent.prompt(message | message[]) overload (agent.ts:371): it starts a run from the given messages.
// Like Pi's prompt([]) (agent.ts:371-379), an empty batch still claims the agent and runs a turn from the current transcript; SendMessages instead returns without a run.
func (a *Agent) PromptMessages(ctx context.Context, messages []AgentMessage) error {
	run, err := a.BeginSendMessages(ctx, messages)
	if err != nil {
		return err
	}
	_, err = run.Run()
	return err
}

// Continue is Pi's agent.continue() (agent.ts:384): it continues from the current transcript and returns nothing but the error. ContinueMessages returns the produced messages as well.
func (a *Agent) Continue(ctx context.Context) error {
	_, err := a.ContinueMessages(ctx)
	return err
}
