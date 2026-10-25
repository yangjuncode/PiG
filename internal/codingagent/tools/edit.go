// 中文导读：edit 工具（EditTool/EditOperations）：文件编辑，回调经 mutation queue 串行化——每个回调完成后队列才前进。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/internal/jsdiff"
	"github.com/MichaelKinsy/PiG/internal/jsonstringify"
	"github.com/MichaelKinsy/PiG/internal/orderedjson"
	"github.com/MichaelKinsy/PiG/internal/text"
)

// ─── Edit Tool ────────────────────────────────────────────────────────────────

type editEntry struct {
	OldText string `json:"oldText"`
	NewText string `json:"newText"`
}

type editParams struct {
	Path  string      `json:"path"`
	Edits []editEntry `json:"edits"`
}

// EditOperations delegates file editing. Each callback completes before the mutation queue advances.
// Ports packages/coding-agent/src/core/tools/edit.ts.
type EditOperations struct {
	ReadFile  func(string) ([]byte, error)
	WriteFile func(string, string) error
	Access    func(string) error
}

// EditTool performs exact-text-replacement edits on files.
// Mirrors upstream packages/coding-agent/src/core/tools/edit.ts.
type EditTool struct {
	CWD        string
	Queue      *FileMutationQueue // serialises concurrent edits
	Operations *EditOperations
}

func (t *EditTool) operations() EditOperations {
	if t.Operations != nil {
		return *t.Operations
	}
	return EditOperations{ReadFile: os.ReadFile, WriteFile: writeFileString, Access: accessReadWrite}
}

func (t *EditTool) Name() string  { return "edit" }
func (t *EditTool) Label() string { return "" }

func (t *EditTool) Schema() ai.ToolSchema {
	return toolSchemaWithParameters(ai.ToolSchema{
		Name:                "edit",
		Description:         "Edit a single file using exact text replacement. Every edits[].oldText must match a unique, non-overlapping region of the original file. If two changes affect the same block or nearby lines, merge them into one edit instead of emitting overlapping edits. Do not include large unchanged regions just to connect distant changes.",
		ConstrainedSampling: strictToolSampling(),
		PromptGuidelines: []string{
			"Use edit for precise changes (edits[].oldText must match exactly)",
			"When changing multiple separate locations in one file, use one edit call with multiple entries in edits[] instead of multiple edit calls",
			"Each edits[].oldText is matched against the original file, not after earlier edits are applied. Do not emit overlapping or nested edits. Merge nearby changes into one edit.",
			"Keep edits[].oldText as small as possible while still being unique in the file. Do not pad with large unchanged regions.",
		},
	}, `{"type":"object","required":["path","edits"],"properties":{
		"path":{"type":"string","description":"Path to the file to edit (relative or absolute)"},
		"edits":{"type":"array","items":{"type":"object","required":["oldText","newText"],"properties":{
			"oldText":{"type":"string","description":"Exact text for one targeted replacement. It must be unique in the original file and must not overlap with any other edits[].oldText in the same call."},
			"newText":{"type":"string","description":"Replacement text for this targeted edit."}
		}},"description":"One or more targeted replacements. Each edit is matched against the original file, not incrementally. Do not include overlapping or nested edits. If two changes touch the same block or nearby lines, merge them into one edit instead."}
	}}`)
}

// ExecutionMode is parallel: upstream's edit definition sets no
// executionMode, and the file mutation queue serialises edits to one file.
func (t *EditTool) ExecutionMode() agent.ToolExecutionMode { return agent.ToolModeParallel }

// RegistersInFileMutationQueue implements agent.RegistersInFileMutationQueue: the parallel dispatcher registers this call in the queue after the extension
// tools before it in the batch started, as Pi runs their synchronous prefixes first.
func (t *EditTool) RegistersInFileMutationQueue() bool { return true }

// ReserveMutationOrder implements agent.QueueOrderable: the parallel
// dispatcher calls this synchronously, in tool-call order, before spawning
// this call's goroutine, so this edit's place in the shared file mutation
// queue is fixed before goroutine scheduling can reorder it. Returns
// ok=false whenever Execute would not reach the queue either (invalid
// params or no edits), so no reservation is ever left un-awaited.
func (t *EditTool) ReserveMutationOrder(rawParams json.RawMessage) (*agent.MutationTicket, bool) {
	if t.Queue == nil {
		return nil, false
	}
	var p editReservation
	if err := json.Unmarshal(prepareEditArgumentsRaw(rawParams), &p); err != nil || len(p.Edits) == 0 {
		return nil, false
	}
	path, err := resolvePath(t.CWD, p.Path)
	if err != nil {
		return nil, false
	}
	ticket, err := t.Queue.Reserve(path)
	if err != nil {
		return nil, false
	}
	return &agent.MutationTicket{Wait: ticket.Wait, Release: ticket.Release}, true
}

// PrepareArguments implements agent.ArgumentPreparer, mirroring upstream
// edit.ts `prepareArguments: prepareEditArguments`: models that send edits
// as a JSON string, as a single edit object, or in the legacy top-level
// {oldText, newText} shape reach the schema validator as {path, edits:[…]}.
// Anything else passes through unchanged.
func (t *EditTool) PrepareArguments(raw json.RawMessage) (json.RawMessage, error) {
	return prepareEditArgumentsRaw(raw), nil
}

// editReservation is editParams for ReserveMutationOrder: it decodes the path and the number of edits, and checks the type of every
// member the full decode reads, without unquoting the texts.
type editReservation struct {
	Path  string `json:"path"`
	Edits []struct {
		OldText unreadString `json:"oldText"`
		NewText unreadString `json:"newText"`
	} `json:"edits"`
}

// memberShape records how often a member was decoded (encoding/json also matches keys that differ only in case) and the first byte
// of its last value.
type memberShape struct {
	seen  int
	first byte
}

func (m *memberShape) UnmarshalJSON(raw []byte) error {
	m.seen++
	m.first = raw[0]
	return nil
}

// editArgumentsShape is the top-level members prepareEditArgumentsRaw rewrites.
type editArgumentsShape struct {
	Edits   memberShape `json:"edits"`
	OldText memberShape `json:"oldText"`
	NewText memberShape `json:"newText"`
}

// prepareEditArgumentsRaw mirrors upstream prepareEditArguments on the raw
// JSON arguments.
func prepareEditArgumentsRaw(raw json.RawMessage) json.RawMessage {
	// Valid arguments with one edits array and no top-level oldText or newText (in any letter case) are left as they are; telling so
	// takes a scan, not the ordered parse below, which copies every member.
	// 快速路径：只数成员出现次数和值的首字节——恰好一个 edits 数组、
	// 且没有顶层 oldText/newText 时就是标准形，原样返回（不付保序
	// 解析成本）。注意 encoding/json 对仅大小写不同的键也会匹配，
	// 与上游宽松语义一致。
	var shape editArgumentsShape
	if json.Unmarshal(raw, &shape) == nil && shape.Edits.seen == 1 && shape.Edits.first == '[' && shape.OldText.seen == 0 && shape.NewText.seen == 0 {
		return raw
	}
	return prepareEditArgumentsOrdered(raw)
}

// prepareEditArgumentsOrdered is prepareEditArgumentsRaw by an ordered parse of the arguments.
func prepareEditArgumentsOrdered(raw json.RawMessage) json.RawMessage {
	args, err := orderedjson.Parse(raw)
	if err != nil {
		return raw
	}
	changed := false
	// Assigning args.edits keeps its position when the member exists and appends it otherwise, as a JS object does.
	// 兼容形一：edits 是 JSON 字符串（模型把数组整体放进了字符串）。
	if editsRaw, ok := args.Get("edits"); ok && isJSONString(editsRaw) {
		var inner string
		if json.Unmarshal(editsRaw, &inner) == nil {
			// JSON.parse skips only JSON whitespace; strings.TrimSpace would also accept \v, \f, U+0085 and U+00A0.
			// 只按 JSON 空白裁剪（JS JSON.parse 口径），再判内容：
			// 是数组 → 直接替换为数组；是单个 edit 对象 → 包成数组。
			switch innerRaw := json.RawMessage(strings.Trim(inner, " \t\n\r")); {
			case json.Valid(innerRaw) && len(innerRaw) > 0 && innerRaw[0] == '[':
				args.Set("edits", innerRaw)
				changed = true
			case json.Valid(innerRaw) && isSingleEditInput(innerRaw):
				args.Set("edits", append(append(json.RawMessage("["), innerRaw...), ']'))
				changed = true
			}
		}
	} else if ok && isSingleEditInput(editsRaw) {
		// 兼容形二：edits 是单个对象而非数组 → 包成数组。
		args.Set("edits", append(append(json.RawMessage("["), editsRaw...), ']'))
		changed = true
	}
	oldRaw, _ := args.Get("oldText")
	newRaw, _ := args.Get("newText")
	// 兼容形三：顶层 {oldText, newText}（旧 schema）→ 折进 edits 数组。
	if isJSONString(oldRaw) && isJSONString(newRaw) {
		// {...rest, edits}: rest keeps its order minus oldText and newText, and edits keeps its place in rest or goes last.
		// 若已有 edits 数组则追加该 edit；保持 JS 对象的键序语义：
		// 删除后重设的 edits 落回原位置（已有键）或末尾。
		edits := []json.RawMessage{}
		if current, ok := args.Get("edits"); ok {
			_ = json.Unmarshal(current, &edits)
		}
		edit := orderedjson.New()
		edit.Set("oldText", oldRaw)
		edit.Set("newText", newRaw)
		editRaw, _ := edit.MarshalJSON()
		edits = append(edits, editRaw)
		list, err := json.Marshal(edits)
		if err != nil {
			return raw
		}
		args.Delete("oldText")
		args.Delete("newText")
		args.Set("edits", list)
		changed = true
	}
	if !changed {
		return raw
	}
	out, err := args.MarshalJSON()
	if err != nil {
		return raw
	}
	// JSON.stringify form: no HTML escaping, integer-like members first.
	// 序列化回 JSON.stringify 形态：不转义 HTML、整数键成员排最前，
	// 与上游重写后的参数逐字节一致。
	canonical, err := jsonstringify.Canonicalize(out)
	if err != nil {
		return raw
	}
	return canonical
}

// isSingleEditInput mirrors upstream isSingleEditInput: a non-array object whose oldText and newText are strings.
func isSingleEditInput(raw json.RawMessage) bool {
	edit, err := orderedjson.Parse(raw)
	if err != nil {
		return false
	}
	oldRaw, _ := edit.Get("oldText")
	newRaw, _ := edit.Get("newText")
	return isJSONString(oldRaw) && isJSONString(newRaw)
}

// isJSONString reports whether raw is a JSON string.
func isJSONString(raw json.RawMessage) bool {
	return len(raw) > 0 && raw[0] == '"'
}

// prepareEditArguments prepares the raw arguments and decodes them.
func prepareEditArguments(raw json.RawMessage) (editParams, error) {
	var p editParams
	if err := json.Unmarshal(prepareEditArgumentsRaw(raw), &p); err != nil {
		return editParams{}, fmt.Errorf("edit: invalid params: %w", err)
	}
	return p, nil
}

// Execute mirrors upstream edit.ts execute: inside the file mutation queue
// it checks access, reads, applies the edits, and writes, checking for an
// abort after each step so the queue is never released mid-write.
func (t *EditTool) Execute(ctx context.Context, _ string, rawParams json.RawMessage, _ agent.ToolUpdateCallback) (agent.AgentToolResult, error) {
	p, err := prepareEditArguments(rawParams)
	if err != nil {
		return agent.AgentToolResult{}, err
	}
	if len(p.Edits) == 0 {
		return editError("Edit tool input is invalid. edits must contain at least one replacement."), nil
	}
	cwd, err := toolCWD(ctx, t.CWD)
	if err != nil {
		return agent.AgentToolResult{}, err
	}
	absPath, err := resolvePath(cwd, p.Path)
	if err != nil {
		return agent.AgentToolResult{}, err
	}

	var result agent.AgentToolResult
	err = runQueued(ctx, t.Queue, absPath, func() error {
		result = t.editLocked(ctx, absPath, p)
		return nil
	})
	if err != nil {
		return editError(err.Error()), nil
	}
	return result, nil
}

func editError(message string) agent.AgentToolResult {
	return agent.AgentToolResult{Content: []ai.ToolResultMessageContent{ai.TextContent{Text: message}}, Details: map[string]any{}, IsError: true, Thrown: true}
}

func (t *EditTool) editLocked(ctx context.Context, absPath string, p editParams) agent.AgentToolResult {
	aborted := func() bool { return ctx.Err() != nil }
	if aborted() {
		return editError("Operation aborted")
	}
	ops := t.operations()
	if err := ops.Access(absPath); err != nil {
		if aborted() {
			return editError("Operation aborted")
		}
		// 上游的错误文案：有 Node error.code 时报 code 而不是
		// 原始错误文本（模型据此区分 ENOENT/EACCES 等）。
		detail := "Error: " + err.Error()
		if code := nodeErrorCode(err); code != "" {
			detail = "Error code: " + code
		}
		return editError(fmt.Sprintf("Could not edit file: %s. %s.", p.Path, detail))
	}
	if aborted() {
		return editError("Operation aborted")
	}
	data, err := ops.ReadFile(absPath)
	if err != nil {
		return editError(NodeFSError(err, "read", ""))
	}
	if aborted() {
		return editError("Operation aborted")
	}
	// 读 → 规范化 → 应用 → 还原行尾：匹配在 LF 规范文本上进行
	// （CRLF 文件的 oldText 也是 LF 形），写回时恢复 BOM 与原始
	// 行尾序列，不悄悄改变文件既有格式。
	var decoder utf8StreamDecoder
	bom, content := text.SplitBom(decoder.decode(data, false))
	originalEnding := detectLineEnding(content)
	normalized := normalizeToLF(content)
	applied, err := applyEditsToNormalizedContent(normalized, p.Edits, p.Path)
	if err != nil {
		return editError(err.Error())
	}
	if aborted() {
		return editError("Operation aborted")
	}
	final := bom + restoreLineEndings(applied.newContent, originalEnding)
	if err := ops.WriteFile(absPath, final); err != nil {
		return editError(NodeFSError(err, "open", absPath))
	}
	if aborted() {
		return editError("Operation aborted")
	}
	// One line diff serves the display diff and the patch, which upstream computes separately over the same pair.
	// 每个步骤后都检查 abort：队列位置要在写完整后释放，但不能
	// 让已取消的调用白跑一次完整读写。
	lineDiff := jsdiff.DiffLineRuns(applied.baseContent, applied.newContent)
	diff, firstChangedLine := diffStringOf(lineDiff, applied.baseContent, applied.newContent, 4)
	patch := unifiedPatchOf(p.Path, lineDiff, 4)
	return agent.AgentToolResult{
		Content: []ai.ToolResultMessageContent{ai.TextContent{Text: fmt.Sprintf("Successfully replaced %d block(s) in %s.", len(p.Edits), p.Path)}},
		Details: &EditToolDetails{Diff: diff, Patch: patch, FirstChangedLine: firstChangedLine},
	}
}
