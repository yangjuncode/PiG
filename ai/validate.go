// Ports packages/ai/src/utils/validation.ts.
package ai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"

	sdkjson "github.com/MichaelKinsy/PiG/extensions/sdk/json"
	"github.com/MichaelKinsy/PiG/internal/jsonstringify"
	"github.com/MichaelKinsy/PiG/internal/typebox"
	"github.com/MichaelKinsy/PiG/third_party/jsonschema"
)

// validateToolArgs clones, normalizes optional nulls, converts, and validates arguments.
// The returned JSON, rather than the model's original input, is passed to hooks and execution. It keeps the member order of args, as Pi validates structuredClone of the arguments and returns it (validation.ts:317-339).
//
// 工具参数进入 Execute 前的最后一道关（prepareToolCall 调用）。不是
// 单纯"验货"而是"整形+验货"：先删掉"可选字段被写成 null"的键
// （normalizeOptionalNulls），再按 TypeBox convert/coerce 语义做类型
// 矫正（字符串 "42"→数字等），最后过 JSON Schema 编译器校验。
// 返回的是矫正后的 JSON——钩子和工具看到的是修整后的参数而非模型
// 原始输入，成员顺序与原始输入保持一致。
func validateToolArgs(toolName string, schema map[string]any, args json.RawMessage) (json.RawMessage, error) {
	if schema == nil {
		schema = map[string]any{}
	}
	//portlint:allow mapkeyorder a map[string]any schema records no authored member order; ValidateToolArguments passes ToolSchema.ParametersJSON and tools with an authored schema reach ValidateToolArgumentsJSON (agent/tool_execution.go:303)
	encoded, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	return validateToolArgsSchema(toolName, encoded, args)
}

// decodeSchemaInstance decodes a JSON text into the value a schema validator checks. sdkjson keeps a lone surrogate escape as one
// UTF-16 unit, as the structuredClone of JSON.parse's result does; encoding/json would hand the tool U+FFFD.
//
// 参数 JSON → 校验用值的解码：刻意用 sdkjson 而非 encoding/json——
// 孤立代理项转义要保留成单个 UTF-16 单元（同 JSON.parse 的
// structuredClone 语义），encoding/json 会把它变成 U+FFFD，
// 模型会看到自己没写过的替换字符。
func decodeSchemaInstance(text json.RawMessage) (any, error) {
	var instance any
	err := sdkjson.Unmarshal(text, &instance)
	return instance, err
}

// validateToolArgsSchema 是带 schema 的完整校验管线：取缓存解析树 →
// 解码参数（保 UTF-16 语义）→ normalizeOptionalNulls → convert →
// coerce → draft-2020 校验 → 失败时按 TypeBox 遍历顺序生成错误。
// 成功返回可能被整形过的参数（marshal 回源成员顺序）。
func validateToolArgsSchema(toolName string, rawSchema, args json.RawMessage) (json.RawMessage, error) {
	entry := toolArgumentSchemas.entry(rawSchema)
	// The parsed schema caches its children and its compiled form as validation reaches them, so one validation at a time uses it.
	entry.mu.Lock()
	defer entry.mu.Unlock()
	schema, err := entry.parsed()
	if err != nil {
		return nil, err
	}
	instance, err := decodeSchemaInstance(args)
	if err != nil {
		return nil, fmt.Errorf("Validation failed for tool %q:\n  - invalid JSON: %w\n\nReceived arguments:\n%s", toolName, err, args)
	}
	// 第一步整形：可选字段被模型写成 null 时直接删键——"没写"和
	// "写了 null"对可选字段是同一语义，留着 null 反而校验不过。
	// The cached schema outlives this validation; it must not keep the arguments' objects reachable after it.
	defer func() { schema.wrapped, schema.mutated = nil, false }()
	schema.normalizeOptionalNulls(instance)
	// Pi ignores Convert's return value. Interior mutations survive; replacing the root does not.
	// 第二步整形：TypeBox convert（仅 ~kind 元数据路径），原地改内部值。
	schema.convert(instance)
	compiled, err := schema.validator()
	if err != nil {
		return nil, err
	}
	coerced := instance
	if schema.value("~skipCoercion") != true {
		// 第三步整形：标准 JSON Schema 的 coerce（字符串"42"→42 等
		// 保守类型矫正）。
		coerced = schema.coerce(instance)
	}
	if !sameArgumentValue(coerced, instance) {
		// coerce 产生了新根值（非原地修改）：对象→对象可以接受替换；
		// 其他类型替换要先用矫正值验一遍，验不过就回退原值继续走
		// 正常校验报错，而不是把矫正值强行塞给工具。
		_, fromObject := instance.(map[string]any)
		_, toObject := coerced.(map[string]any)
		if !fromObject || !toObject {
			if compiled.Validate(coerced) == nil {
				return schema.marshalInSourceOrder(coerced, args)
			}
			return schema.marshalInSourceOrder(instance, args)
		}
		instance = coerced
	}
	if err := compiled.Validate(instance); err != nil {
		// 校验失败：自己走一遍 TypeBox 遍历生成与上游一致措辞的错误
		// 列表（库自带的 ValidationError 格式不同），拼进报错文本。
		failures := schema.argumentErrorsAt(schema.sourceOrder(instance, args), instance, "", "")
		// TypeBox's error context keeps the first maxErrors (8) errors and drops the rest.
		failures = failures[:min(len(failures), typeboxMaxErrors)]
		message := strings.Join(failures, "\n")
		if message == "" {
			message = "Unknown validation error"
		}
		return nil, fmt.Errorf("Validation failed for tool \"%s\":\n%s\n\nReceived arguments:\n%s", toolName, message, prettyArguments(args))
	}
	// Nothing wrote to the decoded arguments, so they are JSON.parse(args); when args is already what JSON.stringify writes for that value, re-encoding it in its own member order returns args.
	if !schema.mutated && len(schema.wrapped) == 0 && jsonstringify.IsCanonical(args) {
		return args, nil
	}
	return schema.marshalInSourceOrder(instance, args)
}

// typeboxMaxErrors is typebox 1.3.27 Settings.maxErrors.
// upstream: node_modules/typebox/build/system/settings/settings.mjs:maxErrors
//
// 单次校验收集错误的数量上限——与 TypeBox Settings.maxErrors 同值：
// 错误报告到 8 条就截断，防止病态 schema/参数爆出无限长错误清单。
const typeboxMaxErrors = 8

// toolArgumentSchemas holds the parsed and compiled schema of each tool whose arguments were validated. A tool call validates against its tool's schema every time, so parsing and compiling it once per distinct schema leaves only the arguments to decode per call.
//
// 参数 schema 的"解析+编译"全局缓存：同一工具的 schema 每次调用都要
// 校验参数，按 schema 原文做 key 只解析编译一次，之后每次调用只剩
// 参数解码的成本。对应上游 WeakMap 缓存，但加了容量上界（D112）。
var toolArgumentSchemas argumentSchemaCache

const (
	// argumentSchemaCacheEntries bounds how many distinct tool schemas stay parsed and compiled; the oldest is dropped first. A session carries a few dozen tools, so 256 holds every tool of a session with room for extension churn; a dropped schema is parsed again on its next call.
	// pig additive (D112): Pi's validator cache is a WeakMap with no bound (validation.ts:6); the bound keeps a long-lived process from retaining a schema per tool definition it ever saw.
	argumentSchemaCacheEntries = 256
	// argumentSchemaCacheMaxBytes is the size of schema text above which a schema is validated without being kept: 1 MiB is far above any tool schema, and one oversized schema must not displace the small ones.
	// pig additive (D112): see argumentSchemaCacheEntries.
	argumentSchemaCacheMaxBytes = 1 << 20
)

// argumentSchemaCache 是带 LRU 上限的 schema 缓存：entries 按原文 key，
// order 记录插入顺序用于淘汰最老条目。
type argumentSchemaCache struct {
	mu      sync.Mutex
	entries map[string]*argumentSchemaEntry
	order   []string
}

// argumentSchemaEntry 是一份 schema 的缓存项：raw 存原文，schema/err
// 是惰性解析结果，done 表示已算过；mu 让并发调用对同一 schema 只解析
// 一次且各自可原地改 wrapped/mutated 状态。
type argumentSchemaEntry struct {
	mu     sync.Mutex
	raw    json.RawMessage
	schema *argumentSchema
	err    error
	done   bool
}

// entry returns the cache entry for the schema text. A schema too large to cache gets an entry of its own that is not kept.
//
// 取缓存项：超过 1MiB 的 schema 不进缓存（造个临时项）——一条巨型
// schema 不该把小 schema 都挤出去；命中上限时淘汰最老（FIFO）。
func (cache *argumentSchemaCache) entry(raw json.RawMessage) *argumentSchemaEntry {
	if len(raw) > argumentSchemaCacheMaxBytes {
		return &argumentSchemaEntry{raw: raw}
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if found := cache.entries[string(raw)]; found != nil {
		return found
	}
	if cache.entries == nil {
		cache.entries = map[string]*argumentSchemaEntry{}
	}
	if len(cache.order) >= argumentSchemaCacheEntries {
		delete(cache.entries, cache.order[0])
		cache.order = cache.order[1:]
	}
	key := string(raw)
	created := &argumentSchemaEntry{raw: json.RawMessage(key)}
	cache.entries[key] = created
	cache.order = append(cache.order, key)
	return created
}

// parsed is parseArgumentSchema of the entry's schema text, computed once. The caller holds entry.mu.
//
// 惰性解析：第一次调用才 parseArgumentSchema，结果（含错误）固化——
// 之后永远返回同一份，调用方须持有 entry.mu。
func (entry *argumentSchemaEntry) parsed() (*argumentSchema, error) {
	if !entry.done {
		entry.schema, entry.err = parseArgumentSchema(entry.raw)
		entry.done = true
	}
	return entry.schema, entry.err
}

// argumentSchema 是参数 schema 的惰性解析树：doc 存解码后的完整节点，
// fields/keys 保留对象成员的原始 JSON 和出现顺序（key 顺序参与
// TypeBox 语义），children 缓存按关键字取出的子 schema，validator
// 缓存编译结果。整套校验（convert/coerce/错误枚举）都在这棵树上
// 递归进行，对应 TypeBox 编译器的运行期行为。
type argumentSchema struct {
	doc        any
	fields     map[string]json.RawMessage
	keys       []string
	children   map[string]*argumentSchema
	compiled   *jsonschema.Schema
	compileErr error
	root       *argumentSchema
	// matchers holds typeboxPropertyNameMatcher of each property name of a "properties" node, compiled when validation first reaches it. The caller holds entry.mu, as for children.
	matchers map[string]func(string) bool
	// wrapped lists the objects Convert wrapped in arrays during the validation in progress, and is empty between validations; only the root carries it, and the caller holds entry.mu.
	wrapped []wrappedObject
	// mutated records that the validation in progress changed a member or item of the arguments (setMember, setItem, a deleted optional null); only the root carries it, and the caller holds entry.mu.
	mutated bool
}

// setMember writes a converted or coerced member back, noting on the root when it is not the value already there.
//
// 回写对象成员并在"值真的变了"时在 root 上置 mutated——convert/coerce
// 的全部原地修改都经这里记账，供外层判断要不要替换整个参数根值。
func (s *argumentSchema) setMember(object map[string]any, key string, value any) {
	if current, ok := object[key]; !ok || !sameArgumentValue(current, value) {
		s.root.mutated = true
	}
	object[key] = value
}

// setItem is setMember for an array item.
//
// setMember 的数组版：写回元素并同样记账 mutated。
func (s *argumentSchema) setItem(items []any, i int, value any) {
	if !sameArgumentValue(items[i], value) {
		s.root.mutated = true
	}
	items[i] = value
}

// parseArgumentSchema 把 schema 原文解析成惰性树：doc=解码值，
// fields=对象成员 raw JSON，keys=成员出现顺序（用流式 Decoder 取，
// Unmarshal 进 map 会丢序——成员顺序参与 TypeBox 的遍历/错误枚举）。
func parseArgumentSchema(raw json.RawMessage) (*argumentSchema, error) {
	s := &argumentSchema{children: map[string]*argumentSchema{}}
	s.root = s
	if err := json.Unmarshal(raw, &s.doc); err != nil {
		return nil, err
	}
	if _, ok := s.doc.(map[string]any); !ok {
		return s, nil
	}
	if err := json.Unmarshal(raw, &s.fields); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		s.keys = append(s.keys, key.(string))
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
	}
	s.keys = argumentObjectKeys(s.keys)
	return s, nil
}

// child 取成员 key 的子 schema（惰性解析+缓存进 children，root 指回
// 根节点共享 wrapped/mutated 状态）；成员不存在或解析失败返回 nil。
func (s *argumentSchema) child(key string) *argumentSchema {
	if child, ok := s.children[key]; ok {
		return child
	}
	raw, ok := s.fields[key]
	if !ok {
		return nil
	}
	child, err := parseArgumentSchema(raw)
	if err != nil {
		return nil
	}
	child.root = s.root
	s.children[key] = child
	return child
}

// list 把成员 key（数组形态的 schema 列表，如 anyOf/oneOf/items 元组）
// 逐项解析成子 schema 切片，同样走 children 缓存（key 为 "字段/下标"）。
func (s *argumentSchema) list(key string) []*argumentSchema {
	var raws []json.RawMessage
	if json.Unmarshal(s.fields[key], &raws) != nil {
		return nil
	}
	out := make([]*argumentSchema, 0, len(raws))
	for i, raw := range raws {
		cacheKey := fmt.Sprintf("%s/%d", key, i)
		child, ok := s.children[cacheKey]
		if !ok {
			child, _ = parseArgumentSchema(raw)
			child.root = s.root
			s.children[cacheKey] = child
		}
		out = append(out, child)
	}
	return out
}

// value/text/types/anySlice 是 schema 节点的便利取值器：
// value=doc 对象成员原值；text=取字符串成员；types=把 type 字段归一成
// []string（单值或联合数组）；anySlice=安全的 []any 断言。
func (s *argumentSchema) value(key string) any {
	object, _ := s.doc.(map[string]any)
	return object[key]
}

func (s *argumentSchema) text(key string) string { value, _ := s.value(key).(string); return value }

func (s *argumentSchema) types() []string {
	if typ := s.text("type"); typ != "" {
		return []string{typ}
	}
	var types []string
	for _, value := range anySlice(s.value("type")) {
		if typ, ok := value.(string); ok {
			types = append(types, typ)
		}
	}
	return types
}

func anySlice(value any) []any { values, _ := value.([]any); return values }

// validator 惰性编译 draft-2020 校验器：注册 TypeBox 格式后经
// CompilerSchema 把老式写法翻译成 draft-2020 再编译，结果（含错误）
// 缓存——同一 schema 只编译一次。
func (s *argumentSchema) validator() (*jsonschema.Schema, error) {
	if s.compiled != nil || s.compileErr != nil {
		return s.compiled, s.compileErr
	}
	compiler := jsonschema.NewCompiler()
	typebox.RegisterFormats(compiler)
	if err := compiler.AddResource("tool.json", typebox.CompilerSchema(s.doc)); err != nil {
		s.compileErr = err
		return nil, err
	}
	s.compiled, s.compileErr = compiler.Compile("tool.json")
	return s.compiled, s.compileErr
}

// check 是"值是否过本 schema"的快速判断：编译失败即 false。
func (s *argumentSchema) check(value any) bool {
	validator, err := s.validator()
	return err == nil && validator.Validate(value) == nil
}

func (s *argumentSchema) normalizeOptionalNulls(value any) {
	if items, ok := value.([]any); ok {
		s.eachItem(items, func(child *argumentSchema, item any) any { child.normalizeOptionalNulls(item); return item })
		return
	}
	object, ok := value.(map[string]any)
	properties := s.child("properties")
	if !ok || properties == nil {
		return
	}
	required := anySlice(s.value("required"))
	for _, key := range properties.keys {
		current, exists := object[key]
		if !exists {
			continue
		}
		child := properties.child(key)
		if current == nil && !slices.Contains(required, any(key)) && child.text("$ref") == "" {
			if validator, err := child.validator(); err == nil && validator.Validate(nil) != nil {
				delete(object, key)
				s.root.mutated = true
				continue
			}
		}
		child.normalizeOptionalNulls(current)
	}
}

// eachItem 遍历数组项并允许 visit 回写矫正值：items 是元组 schema 时
// 按下标对齐逐项处理，是单一 schema 时应用到全部项。
func (s *argumentSchema) eachItem(items []any, visit func(*argumentSchema, any) any) {
	if tuple := s.list("items"); tuple != nil {
		for i := range min(len(items), len(tuple)) {
			s.setItem(items, i, visit(tuple[i], items[i]))
		}
	} else if item := s.child("items"); item != nil {
		if _, ok := item.doc.(map[string]any); ok {
			for i := range items {
				s.setItem(items, i, visit(item, items[i]))
			}
		}
	}
}

// coerce 是"宽容模式"的类型矫正：对没有 ~kind 的普通 JSON Schema，
// 尽量把模型给的近似值整形成 schema 要求的类型（如 "true"→true）。
// 遍历顺序与 TypeBox 一致：先 allOf 逐个子 schema、再 anyOf/oneOf
// 挑能过的分支、然后单 type 矫正、最后递归进 object 属性和 array 元素。
func (s *argumentSchema) coerce(value any) any {
	for _, child := range s.list("allOf") {
		value = child.coerce(value)
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		if children := s.list(key); children != nil {
			value = coerceArgumentUnion(value, children, false)
		}
	}
	types := s.types()
	// 多类型联合（type: ["string","number"]）且当前值已匹配其中一种
	// 时不做矫正；否则按声明顺序试第一种能转的类型。
	matches := len(types) > 1 && slices.ContainsFunc(types, func(typ string) bool { return matchesArgumentType(value, typ) })
	if !matches {
		for _, typ := range types {
			converted := coerceArgumentPrimitive(value, typ, false)
			if !sameArgumentValue(value, converted) {
				value = converted
				break
			}
		}
	}
	if slices.Contains(types, "object") {
		s.eachProperty(value, false)
	}
	if slices.Contains(types, "array") {
		if items, ok := value.([]any); ok {
			s.eachItem(items, func(child *argumentSchema, item any) any { return child.coerce(item) })
		}
	}
	return value
}

// coerceArgumentUnion 处理联合 schema 的矫正：先找已匹配的分支直接
// 返回；否则对每个分支克隆试矫正，native（TypeBox）模式下还要拿矫正
// 结果对整个联合重校验（上游 Convert 的语义），普通 coerce 只校验该
// 分支。全不匹配返回原值。
func coerceArgumentUnion(value any, children []*argumentSchema, native bool) any {
	for _, child := range children {
		if child.check(value) {
			return value
		}
	}
	for _, child := range children {
		candidate := cloneArgumentValue(value)
		if native {
			candidate = child.convert(candidate)
		} else {
			candidate = child.coerce(candidate)
		}
		if native {
			// TypeBox tests each converted candidate against the complete union, not just its arm.
			if slices.ContainsFunc(children, func(arm *argumentSchema) bool { return arm.check(candidate) }) {
				return candidate
			}
		} else if child.check(candidate) {
			return candidate
		}
	}
	return value
}

// typeboxPropertyNameMatcher is Guard.EntriesRegExp's new RegExp(`^${name}$`), unwrapped as TypeBox writes it (so "a|b" is ^a or b$). A name Go cannot compile matches only
// itself; TypeBox throws there.
//
// 复刻上游 Guard.EntriesRegExp 行为：属性名原样拼进 `^${name}$`——
// 名字里含正则元字符时会按正则匹配（"a|b" 实际变成 ^a 或 b$）。
// Go 编译不了的表达式降级成"只匹配自己"（上游此时会抛，分歧点）。
func typeboxPropertyNameMatcher(name string) func(string) bool {
	expression, err := regexp.Compile("^" + name + "$")
	if err != nil {
		return func(key string) bool { return key == name }
	}
	return expression.MatchString
}

// nameMatcher is typeboxPropertyNameMatcher(name), compiled once per schema instead of once per validated object.
//
// 按 schema 缓存编译好的属性名匹配器——上游每校验一个对象编译一次
// 正则，这里按 schema 编译一次复用。
func (s *argumentSchema) nameMatcher(name string) func(string) bool {
	if matcher, ok := s.matchers[name]; ok {
		return matcher
	}
	if s.matchers == nil {
		s.matchers = map[string]func(string) bool{}
	}
	matcher := typeboxPropertyNameMatcher(name)
	s.matchers[name] = matcher
	return matcher
}

// eachProperty 是 convert/coerce 共享的"对象成员遍历+回写"：
// properties 里声明的成员按其子 schema 矫正（native 模式走 TypeBox
// 的 EntriesRegExp 语义——属性名正则匹配到谁就矫正谁，可能不止自己）；
// additionalProperties 兜底处理未声明成员（native 下对每个不匹配已知
// 属性的 key 都矫正一遍，复刻 FromAdditionalProperties）。
func (s *argumentSchema) eachProperty(value any, native bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return
	}
	properties := s.child("properties")
	defined := map[string]bool{}
	if properties != nil && native {
		// TypeBox FromObject (value/convert/from_object.mjs, Guard.EntriesRegExp) tests each property name as new RegExp(`^${name}$`) against every key, so a name with
		// regular-expression characters converts the keys it matches and not necessarily itself.
		keys := slices.Sorted(maps.Keys(object))
		for _, name := range properties.keys {
			defined[name] = true
			child, matches := properties.child(name), properties.nameMatcher(name)
			for _, key := range keys {
				if matches(key) {
					s.setMember(object, key, child.convert(object[key]))
				}
			}
		}
	} else if properties != nil {
		for _, key := range properties.keys {
			defined[key] = true
			current, exists := object[key]
			if !exists {
				continue
			}
			child := properties.child(key)
			if native {
				s.setMember(object, key, child.convert(current))
			} else {
				s.setMember(object, key, child.coerce(current))
			}
		}
	}
	if additional := s.child("additionalProperties"); additional != nil {
		if _, ok := additional.doc.(map[string]any); ok {
			for _, key := range slices.Sorted(maps.Keys(object)) {
				if native {
					// FromAdditionalProperties converts once for every nonmatching known property.
					if properties != nil {
						for _, known := range properties.keys {
							if !properties.nameMatcher(known)(key) {
								s.setMember(object, key, additional.convert(object[key]))
							}
						}
					}
				} else if !defined[key] {
					s.setMember(object, key, additional.coerce(object[key]))
				}
			}
		}
	}
}

// cloneArgumentValue 深拷贝参数值：联合矫正试错时不能污染原参数，
// 先在克隆体上试，成功才采用。
func cloneArgumentValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(value))
		for key, child := range value {
			result[key] = cloneArgumentValue(child)
		}
		return result
	case []any:
		result := make([]any, len(value))
		for i, child := range value {
			result[i] = cloneArgumentValue(child)
		}
		return result
	default:
		return value
	}
}

// sameArgumentValue 的"同一值"判定复刻 JS 语义：map/切片按引用相等
// 比较（JS 的 === 对容器是身份比较，原地矫正后仍"相等"）；标量按值。
func sameArgumentValue(left, right any) bool {
	// JavaScript compares containers by identity; coercion mutates them in place.
	switch left := left.(type) {
	case map[string]any:
		_, ok := right.(map[string]any)
		return ok && reflect.ValueOf(left).Pointer() == reflect.ValueOf(right).Pointer()
	case []any:
		rightSlice, ok := right.([]any)
		return ok && len(left) == len(rightSlice) && reflect.ValueOf(left).Pointer() == reflect.ValueOf(right).Pointer()
	default:
		switch right.(type) {
		case map[string]any, []any:
			return false
		}
		return left == right
	}
}

// prettyArguments 把参数 JSON 美化输出（错误信息里给模型看的参数
// 回显）：规范化失败就回退原文。
func prettyArguments(raw json.RawMessage) string {
	encoded, err := stringifyArgumentJSON(raw)
	if err != nil {
		return string(raw)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, encoded, "", "  "); err != nil {
		return string(raw)
	}
	return pretty.String()
}
