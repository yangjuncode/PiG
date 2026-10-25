package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/MichaelKinsy/PiG/internal/jsstring"
	_ "github.com/MichaelKinsy/PiG/internal/tlsdistrust" // hardens http.DefaultClient (pig additive (D191))

	"github.com/MichaelKinsy/PiG/ai"
)

// ProxyAssistantMessageEventType is the closed `type` union of a proxy event (proxy.ts ProxyAssistantMessageEvent).
//
// 代理事件类型的封闭联合（proxy.ts）：start/xxx_start/delta/end/done/
// error 一组固定取值，对应 provider 端流事件的紧凑形态。
type ProxyAssistantMessageEventType string

// The proxy event types.
const (
	ProxyEventStart         ProxyAssistantMessageEventType = "start"
	ProxyEventTextStart     ProxyAssistantMessageEventType = "text_start"
	ProxyEventTextDelta     ProxyAssistantMessageEventType = "text_delta"
	ProxyEventTextEnd       ProxyAssistantMessageEventType = "text_end"
	ProxyEventThinkingStart ProxyAssistantMessageEventType = "thinking_start"
	ProxyEventThinkingDelta ProxyAssistantMessageEventType = "thinking_delta"
	ProxyEventThinkingEnd   ProxyAssistantMessageEventType = "thinking_end"
	ProxyEventToolcallStart ProxyAssistantMessageEventType = "toolcall_start"
	ProxyEventToolcallDelta ProxyAssistantMessageEventType = "toolcall_delta"
	ProxyEventToolcallEnd   ProxyAssistantMessageEventType = "toolcall_end"
	ProxyEventDone          ProxyAssistantMessageEventType = "done"
	ProxyEventError         ProxyAssistantMessageEventType = "error"
)

// ProxyAssistantMessageEvent is one compact event returned by a proxy server.
// Partial assistant messages are omitted and reconstructed by StreamProxy.
//
// 代理协议的线上事件：代理服务端在 SSE `data:` 行里发送的紧凑 JSON。
// 与 provider 原生流不同，它不重复携带 partial 消息——本侧用
// proxyEventConverter 把 delta 累积进一个共享 partial，再转换成与直连
// provider 完全一致的 ai.AssistantMessageEvent 序列。
type ProxyAssistantMessageEvent struct {
	Type                  ProxyAssistantMessageEventType `json:"type"`
	ContentIndex          int                            `json:"contentIndex"`
	Delta                 string                         `json:"delta"`
	ContentSignature      *string                        `json:"contentSignature,omitempty"`
	ID                    string                         `json:"id"`
	ToolName              string                         `json:"toolName"`
	ToolCall              *ai.ToolCall                   `json:"toolCall,omitempty"`
	Reason                ai.StopReason                  `json:"reason"`
	Usage                 ai.Usage                       `json:"usage"`
	ErrorMessage          *string                        `json:"errorMessage,omitempty"`
	ProviderThinkingLevel *string                        `json:"providerThinkingLevel,omitempty"`

	// toolCallFields records which members the wire toolCall carried, so toolcall_end
	// merges them over the streamed call as Object.assign does.
	toolCallFields map[string]json.RawMessage
	// notArrayIndex marks a contentIndex that is not a JavaScript array index (see parseProxyContentIndex).
	notArrayIndex bool
}

// UnmarshalJSON decodes the event and records the members of a toolcall_end toolCall.
//
// 自定义解码两件事：contentIndex 走 parseProxyContentIndex 按 JS 数组
// 索引语义解析（数字或规范数字字符串）；toolcall_end 的 toolCall 成员
// 表存进 toolCallFields，供 mergeProxyToolCall 判断"线上写了哪些字段"——
// 缺省字段要保留流式累积的值而不是被空值覆盖。
func (event *ProxyAssistantMessageEvent) UnmarshalJSON(data []byte) error {
	type plain ProxyAssistantMessageEvent
	var decoded struct {
		plain
		ContentIndex json.RawMessage `json:"contentIndex"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var members struct {
		ToolCall map[string]json.RawMessage `json:"toolCall"`
	}
	if err := json.Unmarshal(data, &members); err != nil {
		return err
	}
	*event = ProxyAssistantMessageEvent(decoded.plain)
	event.toolCallFields = members.ToolCall
	event.ContentIndex, event.notArrayIndex = parseProxyContentIndex(decoded.ContentIndex)
	return nil
}

// maxProxyArrayIndex is the largest JavaScript array index, 2^32-2.
//
// JS 数组索引上界 2^32-2（数组长度可到 2^32-1，索引到 2^32-2）。
const maxProxyArrayIndex = math.MaxUint32 - 1

// parseProxyContentIndex reads a wire contentIndex. Pi uses the member as a property key of the content array
// (`partial.content[proxyEvent.contentIndex]`), so an index is any value whose property key is an array index: an
// integral number from 0 (-0 included) to 2^32-2, or a string spelling such a number canonically ("0", "17", not "01").
// Any other value (absent, null, negative, fractional, another string) names a non-index property; it reports true.
//
// 线上 contentIndex 的 JS 属性键语义：上游把它当 `partial.content[k]`
// 的下标用，所以"合法索引"= 属性键为数组下标——0（含 -0）到 2^32-2
// 的整数，或规范写法的数字字符串（"0"、"17"，"01" 不算）。其余一切
// （缺省、null、负数、小数、其他字符串）是非索引属性，返回 notArrayIndex。
func parseProxyContentIndex(raw json.RawMessage) (int, bool) {
	// The first byte names the kind, so the value decodes straight into a float64 or a string instead of an interface value.
	switch first := firstJSONByte(raw); {
	case first == '"':
		var key string
		if json.Unmarshal(raw, &key) == nil {
			if index, err := strconv.ParseUint(key, 10, 32); err == nil && index <= maxProxyArrayIndex && index <= math.MaxInt && strconv.FormatUint(index, 10) == key {
				return int(index), false
			}
		}
	case first == '-' || first >= '0' && first <= '9':
		var key float64
		if json.Unmarshal(raw, &key) == nil && key >= 0 && key <= maxProxyArrayIndex && key == math.Trunc(key) {
			return int(key), false
		}
	}
	return -1, true
}

// firstJSONByte is the first byte of text that is not JSON whitespace, or 0.
//
// 跳过 JSON 空白看首字节——单看首字节就能决定按 float64 还是 string
// 解码，省掉 interface{} 通用解码。
func firstJSONByte(text []byte) byte {
	for _, b := range text {
		if b != ' ' && b != '\t' && b != '\n' && b != '\r' {
			return b
		}
	}
	return 0
}

// MarshalJSON emits only the fields carried by the selected proxy event variant.
func (event ProxyAssistantMessageEvent) MarshalJSON() ([]byte, error) {
	type eventType struct {
		Type ProxyAssistantMessageEventType `json:"type"`
	}
	type indexed struct {
		Type         ProxyAssistantMessageEventType `json:"type"`
		ContentIndex int                            `json:"contentIndex"`
	}
	switch event.Type {
	case "start":
		return json.Marshal(eventType{Type: event.Type})
	case "text_start", "thinking_start":
		return json.Marshal(indexed{Type: event.Type, ContentIndex: event.ContentIndex})
	case "text_delta", "thinking_delta", "toolcall_delta":
		return json.Marshal(struct {
			Type         ProxyAssistantMessageEventType `json:"type"`
			ContentIndex int                            `json:"contentIndex"`
			Delta        string                         `json:"delta"`
		}{Type: event.Type, ContentIndex: event.ContentIndex, Delta: event.Delta})
	case "text_end", "thinking_end":
		return json.Marshal(struct {
			Type             ProxyAssistantMessageEventType `json:"type"`
			ContentIndex     int                            `json:"contentIndex"`
			ContentSignature *string                        `json:"contentSignature,omitempty"`
		}{Type: event.Type, ContentIndex: event.ContentIndex, ContentSignature: event.ContentSignature})
	case "toolcall_start":
		return json.Marshal(struct {
			Type         ProxyAssistantMessageEventType `json:"type"`
			ContentIndex int                            `json:"contentIndex"`
			ID           string                         `json:"id"`
			ToolName     string                         `json:"toolName"`
		}{Type: event.Type, ContentIndex: event.ContentIndex, ID: event.ID, ToolName: event.ToolName})
	case "toolcall_end":
		return json.Marshal(struct {
			Type         ProxyAssistantMessageEventType `json:"type"`
			ContentIndex int                            `json:"contentIndex"`
			ToolCall     *ai.ToolCall                   `json:"toolCall"`
		}{Type: event.Type, ContentIndex: event.ContentIndex, ToolCall: event.ToolCall})
	case "done":
		return json.Marshal(struct {
			Type                  ProxyAssistantMessageEventType `json:"type"`
			Reason                ai.StopReason                  `json:"reason"`
			Usage                 ai.Usage                       `json:"usage"`
			ProviderThinkingLevel *string                        `json:"providerThinkingLevel,omitempty"`
		}{Type: event.Type, Reason: event.Reason, Usage: event.Usage, ProviderThinkingLevel: event.ProviderThinkingLevel})
	case "error":
		return json.Marshal(struct {
			Type                  ProxyAssistantMessageEventType `json:"type"`
			Reason                ai.StopReason                  `json:"reason"`
			ErrorMessage          *string                        `json:"errorMessage,omitempty"`
			Usage                 ai.Usage                       `json:"usage"`
			ProviderThinkingLevel *string                        `json:"providerThinkingLevel,omitempty"`
		}{
			Type: event.Type, Reason: event.Reason, ErrorMessage: event.ErrorMessage,
			Usage: event.Usage, ProviderThinkingLevel: event.ProviderThinkingLevel,
		})
	default:
		return json.Marshal(eventType{Type: event.Type})
	}
}

// ProxyStreamOptions configures StreamProxy. Context cancellation is the Go
// equivalent of upstream's optional AbortSignal.
// Non-nil maps retain empty objects. ThinkingBudgets preserves each budget's presence, including explicit zeros.
type ProxyStreamOptions struct {
	Temperature     *float64
	SamplingParams  map[string]any
	MaxTokens       *int
	Reasoning       ai.ModelThinkingLevel
	CacheRetention  ai.CacheRetention
	SessionID       string
	Headers         ai.ProviderHeaders
	Metadata        map[string]any
	Transport       ai.Transport
	ThinkingBudgets *ProxyThinkingBudgets
	MaxRetryDelayMs *int
	AuthToken       string
	ProxyURL        string
}

type proxySerializableStreamOptions struct {
	Temperature     *float64              `json:"temperature,omitempty"`
	SamplingParams  map[string]any        `json:"samplingParams,omitzero"`
	MaxTokens       *int                  `json:"maxTokens,omitempty"`
	Reasoning       ai.ModelThinkingLevel `json:"reasoning,omitempty"`
	CacheRetention  ai.CacheRetention     `json:"cacheRetention,omitempty"`
	SessionID       string                `json:"sessionId,omitempty"`
	Headers         ai.ProviderHeaders    `json:"headers,omitzero"`
	Metadata        map[string]any        `json:"metadata,omitzero"`
	Transport       ai.Transport          `json:"transport,omitempty"`
	ThinkingBudgets *ProxyThinkingBudgets `json:"thinkingBudgets,omitempty"`
	MaxRetryDelayMs *int                  `json:"maxRetryDelayMs,omitempty"`
}

// ProxyThinkingBudgets carries optional per-level token budgets to the proxy server.
// Nil fields stay omitted; non-nil fields send their value, including zero.
// A non-nil empty struct sends an empty object without overriding server defaults.
type ProxyThinkingBudgets struct {
	Minimal *int `json:"minimal,omitempty"`
	Low     *int `json:"low,omitempty"`
	Medium  *int `json:"medium,omitempty"`
	High    *int `json:"high,omitempty"`
}

type proxyModel struct {
	ID               string               `json:"id"`
	Name             string               `json:"name"`
	API              ai.API               `json:"api"`
	Provider         string               `json:"provider"`
	BaseURL          string               `json:"baseUrl"`
	Reasoning        bool                 `json:"reasoning"`
	ThinkingLevelMap ai.ThinkingLevelMap  `json:"thinkingLevelMap,omitempty"`
	Input            []string             `json:"input"`
	InputLimits      *ai.ModelInputLimits `json:"inputLimits,omitempty"`
	Cost             ai.ModelCost         `json:"cost"`
	PromptCache      ai.ModelPromptCache  `json:"promptCache,omitempty"`
	ContextWindow    int                  `json:"contextWindow"`
	MaxTokens        int                  `json:"maxTokens"`
	SamplingParams   map[string]any       `json:"samplingParams,omitempty"`
	// SamplingParamsByThinkingLevel is Model.samplingParamsByThinkingLevel; the proxy request carries the whole model.
	SamplingParamsByThinkingLevel ai.SamplingParamsByThinkingLevel `json:"samplingParamsByThinkingLevel,omitempty"`
	Headers                       map[string]string                `json:"headers,omitempty"`
	Compat                        *ai.ModelCompat                  `json:"compat,omitempty"`
}

type proxyRequestPayload struct {
	Model   proxyModel                     `json:"model"`
	Context proxyRequestContext            `json:"context"`
	Options proxySerializableStreamOptions `json:"options"`
}

type proxyRequestContext struct {
	Messages []ai.Message `json:"messages"`
}

// StreamProxy streams through a server that manages provider authentication.
// Each toolcall_start replaces the indexed tool and its accumulated JSON.
// It returns immediately; request, protocol, and cancellation failures are
// delivered as terminal error events.
//
// 代理模式的 StreamFn：模型元信息+消息上下文+选项打包成一次 POST
// 发到 {ProxyURL}/api/stream，响应是 SSE 行流。同步部分只做请求体
// 构建（失败直接入 stream 报错），然后立即返回事件流；goroutine 里
// 读行、转换、Push，直到 done/error 或连接断开——未收到终结事件就
// EOF 也算 error 事件，保证消费方总能等到终止信号。
func StreamProxy(ctx context.Context, model *ai.Model, transcript ai.TranscriptContext, options ProxyStreamOptions) *ai.AssistantMessageEventStream {
	stream := ai.NewAssistantMessageEventStream()
	partial := newProxyPartial(model)
	converter := &proxyEventConverter{partial: partial, toolJSON: map[int]string{}}
	body, err := buildProxyRequest(model, transcript, options)
	go func(runErr error) {
		if runErr == nil {
			runErr = runProxyRequest(ctx, options, body, converter, stream)
		}
		if runErr != nil {
			pushProxyFailure(stream, partial, runErr, ctx != nil && ctx.Err() != nil)
		}
	}(err)
	return stream
}

func newProxyPartial(model *ai.Model) *ai.AssistantMessage {
	partial := &ai.AssistantMessage{
		Content: []ai.AssistantContentBlock{}, StopReason: ai.StopReasonPending, Timestamp: time.Now().UnixMilli(),
	}
	if model == nil {
		return partial
	}
	partial.API = model.ProviderMeta.API
	partial.Provider = proxyProviderID(model)
	partial.Model = model.ID
	return partial
}

func proxyProviderID(model *ai.Model) string {
	if model.ProviderMeta.ProviderID != "" {
		return model.ProviderMeta.ProviderID
	}
	if model.Provider != nil {
		return model.Provider.ID()
	}
	return ""
}

func buildProxyRequest(model *ai.Model, transcript ai.TranscriptContext, options ProxyStreamOptions) ([]byte, error) {
	if model == nil {
		return nil, errors.New("proxy model is nil")
	}
	messages := transcript.Messages()
	if messages == nil {
		messages = []ai.Message{}
	}
	payload := proxyRequestPayload{
		Model: proxyModel{
			ID: model.ID, Name: model.DisplayName, API: model.ProviderMeta.API, Provider: proxyProviderID(model),
			BaseURL: model.ProviderMeta.BaseURL, Reasoning: model.ProviderMeta.Reasoning,
			ThinkingLevelMap: model.ThinkingLevelMap, Input: model.Input, InputLimits: model.InputLimits,
			Cost: model.CostRates(), PromptCache: model.PromptCache,
			ContextWindow: model.Capabilities.ContextWindow, MaxTokens: model.Capabilities.MaxOutputTokens,
			SamplingParams: model.SamplingParams, SamplingParamsByThinkingLevel: model.SamplingParamsByThinkingLevel, Headers: model.ProviderMeta.Headers, Compat: model.ProviderMeta.Compat,
		},
		Context: proxyRequestContext{Messages: messages},
		Options: proxySerializableStreamOptions{
			Temperature: options.Temperature, SamplingParams: options.SamplingParams, MaxTokens: options.MaxTokens,
			Reasoning: options.Reasoning, CacheRetention: options.CacheRetention, SessionID: options.SessionID,
			Headers: options.Headers, Metadata: options.Metadata, Transport: options.Transport,
			ThinkingBudgets: options.ThinkingBudgets, MaxRetryDelayMs: options.MaxRetryDelayMs,
		},
	}
	return json.Marshal(payload)
}

func runProxyRequest(ctx context.Context, options ProxyStreamOptions, body []byte, converter *proxyEventConverter, stream *ai.AssistantMessageEventStream) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, options.ProxyURL+"/api/stream", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+options.AuthToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			// upstream: packages/agent/src/proxy.ts:streamProxy fetch rejects with the signal's reason, a DOMException AbortError by default
			return proxyAbortReason(ctx)
		}
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return proxyHTTPError(response)
	}
	return readProxyEvents(ctx, response.Body, converter, stream)
}

// proxyAbortReason is the message fetch rejects with when the signal aborts before the response arrives: a cancel cause, the
// TimeoutError of AbortSignal.timeout for an expired deadline, or the default AbortError.
//
// 响应还没回来就被 abort 时 fetch 的拒绝原因：有 cancel cause 用它
// （含 AbortSignal.timeout 到期的 TimeoutError）；普通取消归为
// AbortError（上游 fetch 的默认拒绝形态）。
func proxyAbortReason(ctx context.Context) error {
	cause := context.Cause(ctx)
	switch {
	case cause == nil || errors.Is(cause, context.Canceled):
		return errors.New("This operation was aborted")
	case errors.Is(cause, context.DeadlineExceeded):
		return errors.New("The operation was aborted due to timeout")
	default:
		return cause
	}
}

func proxyHTTPError(response *http.Response) error {
	var payload struct {
		Error string `json:"error"`
	}
	if json.NewDecoder(response.Body).Decode(&payload) == nil && payload.Error != "" {
		return errors.New("Proxy error: " + payload.Error)
	}
	return errors.New("Proxy error: " + response.Status)
}

type proxyEventConverter struct {
	partial  *ai.AssistantMessage
	toolJSON map[int]string
	// streamed appends deltas to the open blocks without copying their earlier bytes again and parses tool arguments from where the last delta ended.
	streamed ai.BlockDeltas
}

func readProxyEvents(ctx context.Context, body io.Reader, converter *proxyEventConverter, stream *ai.AssistantMessageEventStream) error {
	reader := bufio.NewReader(body)
	for {
		// SSE 逐行读：每行可能是一个 data: 事件；ctx 取消立即退出。
		line, readErr := reader.ReadString('\n')
		if ctx.Err() != nil {
			return errors.New("Request aborted by user")
		}
		if line != "" {
			terminal, err := processProxyLine(line, converter, stream)
			if err != nil {
				return err
			}
			if terminal {
				// pig divergence (D183): the terminal event hands partial to the consumer, so nothing may change it afterwards. Pi keeps reading and
				// applies later events, parse failures and aborts to the delivered message; in Go that would be a data race.
				// The rest of the body is drained without effect so the connection lives as long as Pi's.
				_, _ = io.Copy(io.Discard, reader)
				return nil
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return readErr
		}
	}
	if ctx.Err() != nil {
		return errors.New("Request aborted by user")
	}
	// 连接干净关闭但没收到 done/error：视为服务端半途断线，补一个
	// error 终结事件——消费方（consumeStream）必须见到终结事件。
	converter.partial.StopReason = ai.StopReasonError
	converter.partial.ErrorMessage = "Connection closed by proxy server before the response completed"
	return stream.Push(ai.ErrorEvent{Reason: ai.StopReasonError, Error: converter.partial})
}

func processProxyLine(line string, converter *proxyEventConverter, stream *ai.AssistantMessageEventStream) (bool, error) {
	// upstream: packages/agent/src/proxy.ts:processLine
	// 只认 `data: ` 前缀的 SSE 行，其他行（注释、事件名行）忽略。
	if !strings.HasPrefix(line, "data: ") {
		return false, nil
	}
	// upstream: packages/agent/src/proxy.ts:processLine
	data := jsstring.Trim(strings.TrimPrefix(line, "data: "))
	if data == "" {
		return false, nil
	}
	var proxyEvent ProxyAssistantMessageEvent
	if err := json.Unmarshal([]byte(data), &proxyEvent); err != nil {
		return false, err
	}
	// 把代理紧凑事件累积进 partial 并还原成标准 provider 事件；
	// 返回 nil 表示该行不产生事件（未知类型）。
	event, err := converter.process(proxyEvent)
	if err != nil || event == nil {
		return false, err
	}
	terminal := event.EventType() == ai.EventDone || event.EventType() == ai.EventError
	if err := stream.Push(event); err != nil {
		return false, err
	}
	return terminal, nil
}

func (converter *proxyEventConverter) process(proxyEvent ProxyAssistantMessageEvent) (ai.AssistantMessageEvent, error) {
	partial := converter.partial
	if proxyEvent.notArrayIndex && proxyEventStoresContent(proxyEvent.Type) {
		// pig divergence (D182): Pi stores the block as a non-index property of the content array, which JSON never shows; an ai.AssistantMessage has no such slot, so the event fails.
		return nil, fmt.Errorf("proxy %s event has no usable contentIndex", proxyEvent.Type)
	}
	switch proxyEvent.Type {
	case "start":
		return ai.StartEvent{Partial: partial}, nil
	case "text_start":
		if err := setProxyContent(partial, proxyEvent.ContentIndex, ai.TextContent{}); err != nil {
			return nil, err
		}
		return ai.TextStartEvent{ContentIndex: proxyEvent.ContentIndex, Partial: partial}, nil
	case "text_delta":
		content, err := proxyContent(partial, proxyEvent.ContentIndex)
		text, ok := content.(ai.TextContent)
		if err != nil || !ok {
			return nil, errors.New("Received text_delta for non-text content")
		}
		text.Text = converter.streamed.AppendContent(proxyEvent.ContentIndex, text.Text, proxyEvent.Delta)
		partial.Content[proxyEvent.ContentIndex] = text
		return ai.TextDeltaEvent{ContentIndex: proxyEvent.ContentIndex, Delta: proxyEvent.Delta, Partial: partial}, nil
	case "text_end":
		content, err := proxyContent(partial, proxyEvent.ContentIndex)
		text, ok := content.(ai.TextContent)
		if err != nil || !ok {
			return nil, errors.New("Received text_end for non-text content")
		}
		text.TextSignature = optionalProxyString(proxyEvent.ContentSignature)
		converter.streamed.Release(proxyEvent.ContentIndex)
		partial.Content[proxyEvent.ContentIndex] = text
		return ai.TextEndEvent{ContentIndex: proxyEvent.ContentIndex, Content: text.Text, Partial: partial}, nil
	case "thinking_start":
		if err := setProxyContent(partial, proxyEvent.ContentIndex, ai.ThinkingContent{}); err != nil {
			return nil, err
		}
		return ai.ThinkingStartEvent{ContentIndex: proxyEvent.ContentIndex, Partial: partial}, nil
	case "thinking_delta":
		content, err := proxyContent(partial, proxyEvent.ContentIndex)
		thinking, ok := content.(ai.ThinkingContent)
		if err != nil || !ok {
			return nil, errors.New("Received thinking_delta for non-thinking content")
		}
		thinking.Thinking = converter.streamed.AppendContent(proxyEvent.ContentIndex, thinking.Thinking, proxyEvent.Delta)
		partial.Content[proxyEvent.ContentIndex] = thinking
		return ai.ThinkingDeltaEvent{ContentIndex: proxyEvent.ContentIndex, Delta: proxyEvent.Delta, Partial: partial}, nil
	case "thinking_end":
		content, err := proxyContent(partial, proxyEvent.ContentIndex)
		thinking, ok := content.(ai.ThinkingContent)
		if err != nil || !ok {
			return nil, errors.New("Received thinking_end for non-thinking content")
		}
		thinking.ThinkingSignature = optionalProxyString(proxyEvent.ContentSignature)
		converter.streamed.Release(proxyEvent.ContentIndex)
		partial.Content[proxyEvent.ContentIndex] = thinking
		return ai.ThinkingEndEvent{ContentIndex: proxyEvent.ContentIndex, Content: thinking.Thinking, Partial: partial}, nil
	case "toolcall_start":
		// 新工具调用块：重置该 index 的参数 JSON 累积器——同一 index
		// 可能被复用，旧累积必须清掉。
		toolCall := ai.ToolCall{ID: proxyEvent.ID, Name: proxyEvent.ToolName, Arguments: ai.JsonObject{}}
		toolCall.SetPartialJson("")
		if err := setProxyContent(partial, proxyEvent.ContentIndex, toolCall); err != nil {
			return nil, err
		}
		delete(converter.toolJSON, proxyEvent.ContentIndex)
		converter.streamed.Release(proxyEvent.ContentIndex)
		return ai.ToolCallStartEvent{ContentIndex: proxyEvent.ContentIndex, Partial: partial}, nil
	case "toolcall_delta":
		// 参数 JSON 按 delta 逐段累积进 toolJSON，再喂给 ToolCall 的
		// 流式参数解析（尽力而为的部分解析）。
		content, err := proxyContent(partial, proxyEvent.ContentIndex)
		toolCall, ok := content.(ai.ToolCall)
		if err != nil || !ok {
			return nil, errors.New("Received toolcall_delta for non-toolCall content")
		}
		converter.toolJSON[proxyEvent.ContentIndex] = converter.streamed.AppendArguments(proxyEvent.ContentIndex, converter.toolJSON[proxyEvent.ContentIndex], proxyEvent.Delta)
		toolCall.SetPartialJson(converter.toolJSON[proxyEvent.ContentIndex])
		converter.streamed.SetStreamingArguments(&toolCall, proxyEvent.ContentIndex, converter.toolJSON[proxyEvent.ContentIndex])
		// proxy.ts:343: parseStreamingJson(partialJson) || {}.
		toolCall.DefaultFalsyArguments()
		partial.Content[proxyEvent.ContentIndex] = toolCall
		return ai.ToolCallDeltaEvent{ContentIndex: proxyEvent.ContentIndex, Delta: proxyEvent.Delta, Partial: partial}, nil
	case "toolcall_end":
		content, err := proxyContent(partial, proxyEvent.ContentIndex)
		toolCall, ok := content.(ai.ToolCall)
		if err != nil || !ok {
			return nil, nil
		}
		if proxyEvent.ToolCall != nil {
			toolCall = mergeProxyToolCall(toolCall, *proxyEvent.ToolCall, proxyEvent.toolCallFields)
		}
		toolCall.DeletePartialJson()
		partial.Content[proxyEvent.ContentIndex] = toolCall
		delete(converter.toolJSON, proxyEvent.ContentIndex)
		converter.streamed.Release(proxyEvent.ContentIndex)
		return ai.ToolCallEndEvent{ContentIndex: proxyEvent.ContentIndex, ToolCall: toolCall, Partial: partial}, nil
	case "done":
		finishProxyPartial(partial, proxyEvent)
		return ai.DoneEvent{Reason: proxyEvent.Reason, Message: partial}, nil
	case "error":
		finishProxyPartial(partial, proxyEvent)
		partial.ErrorMessage = optionalProxyString(proxyEvent.ErrorMessage)
		return ai.ErrorEvent{Reason: proxyEvent.Reason, Error: partial}, nil
	default:
		// upstream: packages/agent/src/proxy.ts:processProxyEvent console.warn
		fmt.Fprintf(os.Stderr, "Unhandled proxy event type: %s\n", proxyEvent.Type)
		return nil, nil
	}
}

// mergeProxyToolCall mirrors Object.assign(content, proxyEvent.toolCall): members the wire call omits keep the streamed call's value.
//
// 对应上游 Object.assign(content, proxyEvent.toolCall)：线上 toolCall
// 没写的成员（id/name/arguments）保留流式累积值——toolcall_end 常只带
// 增量片段，不能让它把前面 delta 攒下的字段抹掉。
func mergeProxyToolCall(streamed, wire ai.ToolCall, fields map[string]json.RawMessage) ai.ToolCall {
	if _, ok := fields["id"]; !ok {
		wire.ID = streamed.ID
	}
	if _, ok := fields["name"]; !ok {
		wire.Name = streamed.Name
	}
	if _, ok := fields["arguments"]; !ok {
		wire.CopyArgumentsFrom(streamed)
	}
	return wire
}

// proxyEventStoresContent reports the events that write `partial.content[index]`. The others only read it, and a read at a
// non-index key finds nothing in Pig, as in Pi: a store at such a key has already ended the stream.
//
// 会写 `partial.content[index]` 的只有三种 start 事件（text/thinking/
// toolcall）；其余事件只读。非索引键上的读在 Pi 与 PiG 里都拿不到
// 东西——那种键上的写早把流终结了。
func proxyEventStoresContent(eventType ProxyAssistantMessageEventType) bool {
	switch eventType {
	case ProxyEventTextStart, ProxyEventThinkingStart, ProxyEventToolcallStart:
		return true
	}
	return false
}

// pig divergence (D182): setProxyContent stores a block at an index as `partial.content[index] = block` does, except that an index past the end is an error: Pi leaves holes in the array, and a nil block cannot live in an ai.AssistantMessage (its copy, marshalling and estimation take only the three block kinds).
func setProxyContent(partial *ai.AssistantMessage, index int, content ai.AssistantContentBlock) error {
	if index > len(partial.Content) {
		return fmt.Errorf("proxy content index %d is past the end of the content array", index)
	}
	if index == len(partial.Content) {
		partial.Content = append(partial.Content, content)
	} else {
		partial.Content[index] = content
	}
	return nil
}

// proxyContent reads `partial.content[index]`: a missing block (a non-index key is -1) is undefined, which no event
// handler accepts.
func proxyContent(partial *ai.AssistantMessage, index int) (ai.AssistantContentBlock, error) {
	if index < 0 || index >= len(partial.Content) {
		return nil, nil
	}
	return partial.Content[index], nil
}

func optionalProxyString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func finishProxyPartial(partial *ai.AssistantMessage, event ProxyAssistantMessageEvent) {
	partial.StopReason = event.Reason
	partial.Usage = event.Usage
	if event.ProviderThinkingLevel != nil {
		partial.ProviderThinkingLevel = *event.ProviderThinkingLevel
	}
}

func pushProxyFailure(stream *ai.AssistantMessageEventStream, partial *ai.AssistantMessage, err error, aborted bool) {
	reason := ai.StopReasonError
	if aborted {
		reason = ai.StopReasonAborted
	}
	partial.StopReason = reason
	partial.ErrorMessage = err.Error()
	_ = stream.Push(ai.ErrorEvent{Reason: reason, Error: partial})
}
