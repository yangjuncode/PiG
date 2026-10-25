// 中文导读：partial assistant 消息的发布注册表。
//
// 流式事件把 producer 的 partial 消息交给多个消费者。为了不共享
// 可变构造数据、又不为每个消费者复制整棵消息，stream 为每条源
// 消息维护一个 cell（assistantMessageCell），并用 weak.Pointer +
// runtime.Cleanup 在消费者释放视图后自动清理注册项。注意 D82
// 差异：executor 之外的 producer 只能拿到发布时刻的不可变快照，
// 因为活视图会让读者看到自由运行的 goroutine 当时的任意修订。
package ai

import (
	"runtime"
	"weak"
)

type assistantMessagePublication struct {
	cell    weak.Pointer[assistantMessageCell]
	cleanup runtime.Cleanup
}

type assistantPublications map[weak.Pointer[AssistantMessage]]*assistantMessagePublication

type assistantPublicationCleanup struct {
	stream  weak.Pointer[AssistantMessageEventStream]
	message weak.Pointer[AssistantMessage]
}

func cleanupAssistantPublication(cleanup assistantPublicationCleanup) {
	if stream := cleanup.stream.Value(); stream != nil {
		stream.mu.Lock()
		delete(stream.publications, cleanup.message)
		stream.mu.Unlock()
	}
}

// The registry preserves source-object identity without retaining consumed messages or mutating caller-owned public structs. Builders publish their own cells; forwarded views keep the original cell.
func (stream *AssistantMessageEventStream) publishPartialLocked(message *AssistantMessage, replacements assistantMessageReplacements) *AssistantMessage {
	if message == nil || message.observation != nil {
		return message
	}
	if stream.producer == nil {
		// pig divergence (D82): a producer outside the JavaScript-order executor publishes an immutable emission-time snapshot. A live view would expose whatever revision a free-running goroutine reached when a consumer happened to observe it.
		// A tool call whose arguments hold a long string that grows is not validated from its start at each push: the string the last snapshot found valid is the prefix the next one skips (plainWalk.knownValid).
		validated := ""
		snapshot := cloneAssistantMessageWith(*message, func(value any) any {
			walk := plainWalk{knownValid: stream.validated}
			copied, ok := clonePlainJSON(value, &walk)
			if !ok {
				return cloneJSONValue(value)
			}
			if len(walk.longest) > len(validated) {
				validated = walk.longest
			}
			return copied
		})
		stream.validated = validated
		return &snapshot
	}
	if stream.publications == nil {
		stream.publications = make(assistantPublications)
	}
	key := weak.Make(message)
	publication := stream.publications[key]
	if publication == nil {
		publication = &assistantMessagePublication{}
		publication.cleanup = runtime.AddCleanup(message, cleanupAssistantPublication, assistantPublicationCleanup{stream: weak.Make(stream), message: key})
		stream.publications[key] = publication
	}
	cell := publication.cell.Value()
	if cell == nil {
		cell = newAssistantMessageCell(message)
		publication.cell = weak.Make(cell)
	} else {
		cell.publish(message, replacements)
	}
	view := cell.view()
	runtime.KeepAlive(message)
	return view
}

func (stream *AssistantMessageEventStream) publishTerminalLocked(message *AssistantMessage, replacements assistantMessageReplacements) {
	if message == nil {
		return
	}
	if publication := stream.publications[weak.Make(message)]; publication != nil {
		if cell := publication.cell.Value(); cell != nil {
			cell.publish(message, replacements)
		}
	}
	runtime.KeepAlive(message)
}

func (stream *AssistantMessageEventStream) clearPublicationsLocked() {
	for _, publication := range stream.publications {
		publication.cleanup.Stop()
	}
	stream.publications = nil
}
