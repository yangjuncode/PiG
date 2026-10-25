package ai

import (
	"fmt"
	"math"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// argumentErrorsAt follows TypeBox 1.3.27 schema/engine/schema.mjs and system/locale/en_US.mjs. Unlike jsonschema.ValidationError it reports siblings after a type failure and includes failing union arms before the
// union error. It takes the JSON pointer of the value and the member order of the arguments' source: TypeBox visits object members in their enumeration order, so errors for additional
// properties, pattern properties and property names follow it.
//
// 校验失败时的错误报告生成器：手写遍历而不是读 jsonschema 库的
// ValidationError，因为返回给模型的错误文本必须与上游 TypeBox 输出
// 一致（措辞、路径格式、遍历顺序——union 要先列每个分支的失败再报
// union 总错）。模型读到这些文本后会按提示修正参数重试。
func (s *argumentSchema) argumentErrorsAt(order schemaObjectOrder, value any, path, pointer string) []string {
	var lines []string
	emit := func(message string) {
		location := path
		if location == "" {
			location = "root"
		}
		lines = append(lines, "  - "+location+": "+message)
	}
	nested := func(child *argumentSchema, item any, nextPath, nextPointer string) bool {
		failures := child.argumentErrorsAt(order, item, nextPath, nextPointer)
		lines = append(lines, failures...)
		return len(failures) == 0
	}
	if boolean, ok := s.doc.(bool); ok {
		if !boolean {
			emit("schema is false")
		}
		return lines
	}
	types := s.types()
	if len(types) > 0 && !slices.ContainsFunc(types, func(typ string) bool { return matchesArgumentType(value, typ) }) {
		if _, union := s.value("type").([]any); union {
			emit("must be either " + strings.Join(types, " or "))
		} else {
			emit("must be " + types[0])
		}
	}
	if object, ok := value.(map[string]any); ok {
		var missing []string
		for _, required := range anySlice(s.value("required")) {
			key, _ := required.(string)
			if _, exists := object[key]; !exists {
				missing = append(missing, key)
			}
		}
		if len(missing) > 0 {
			location := argumentPath(path, missing[0])
			lines = append(lines, "  - "+location+": must have required properties "+strings.Join(missing, ", "))
		}
		properties := s.child("properties")
		patterns := s.child("patternProperties")
		if additional := s.child("additionalProperties"); additional != nil {
			failed := false
			for _, key := range orderedSchemaKeys(object, order, pointer) {
				known := properties != nil && properties.child(key) != nil
				if patterns != nil {
					for _, pattern := range patterns.keys {
						if argumentPatternMatches(pattern, key) {
							known = true
						}
					}
				}
				if !known && !nested(additional, object[key], argumentPath(path, argumentKeySegment(key)), pointer+"/"+escapeArgumentPointer(key)) {
					failed = true
				}
			}
			if failed {
				emit("must not have additional properties")
			}
		}
		for _, keyword := range []string{"dependencies", "dependentRequired", "dependentSchemas"} {
			if dependencies := s.child(keyword); dependencies != nil {
				for _, key := range dependencies.keys {
					if _, exists := object[key]; !exists {
						continue
					}
					child := dependencies.child(key)
					if required, ok := child.doc.([]any); ok {
						var missing []string
						for _, name := range required {
							key, _ := name.(string)
							if _, exists := object[key]; !exists {
								missing = append(missing, key)
							}
						}
						if len(missing) > 0 {
							emit(fmt.Sprintf("must have properties %s when property %s is present", strings.Join(missing, ", "), key))
						}
					} else {
						nested(child, value, path, pointer)
					}
				}
			}
		}
		if patterns != nil {
			for _, pattern := range patterns.keys {
				for _, key := range orderedSchemaKeys(object, order, pointer) {
					if argumentPatternMatches(pattern, key) {
						nested(patterns.child(pattern), object[key], argumentPath(path, argumentKeySegment(key)), pointer+"/"+escapeArgumentPointer(key))
					}
				}
			}
		}
		if properties != nil {
			for _, key := range properties.keys {
				if item, exists := object[key]; exists {
					nested(properties.child(key), item, argumentPath(path, argumentKeySegment(key)), pointer+"/"+escapeArgumentPointer(key))
				}
			}
		}
		if names := s.child("propertyNames"); names != nil {
			var invalid []string
			for _, key := range orderedSchemaKeys(object, order, pointer) {
				if !nested(names, key, argumentPath(path, argumentKeySegment(key)), pointer+"/"+escapeArgumentPointer(key)) {
					invalid = append(invalid, key)
				}
			}
			if len(invalid) > 0 {
				emit("property names " + strings.Join(invalid, ", ") + " are invalid")
			}
		}
		s.argumentLimitErrors(float64(len(object)), "minProperties", "maxProperties", "properties", emit)
	}
	if items, ok := value.([]any); ok {
		if tuple := s.list("items"); tuple != nil {
			if additional := s.child("additionalItems"); additional != nil {
				for i := len(tuple); i < len(items); i++ {
					nested(additional, items[i], argumentPath(path, fmt.Sprint(i)), pointer+"/"+fmt.Sprint(i))
				}
			}
		}
		if contains := s.child("contains"); contains != nil {
			count := 0
			for i, item := range items {
				if len(contains.argumentErrorsAt(order, item, argumentPath(path, fmt.Sprint(i)), pointer+"/"+fmt.Sprint(i))) == 0 {
					count++
				}
			}
			if count == 0 {
				emit("must contain at least 1 valid item")
			}
		}
		if tuple := s.list("items"); tuple != nil {
			for i := range min(len(tuple), len(items)) {
				nested(tuple[i], items[i], argumentPath(path, fmt.Sprint(i)), pointer+"/"+fmt.Sprint(i))
			}
		} else if child := s.child("items"); child != nil {
			for i, item := range items {
				nested(child, item, argumentPath(path, fmt.Sprint(i)), pointer+"/"+fmt.Sprint(i))
			}
		}
		s.argumentLimitErrors(float64(len(items)), "minItems", "maxItems", "items", emit)
		if prefix := s.list("prefixItems"); prefix != nil {
			for i := range min(len(prefix), len(items)) {
				nested(prefix[i], items[i], argumentPath(path, fmt.Sprint(i)), pointer+"/"+fmt.Sprint(i))
			}
		}
		if s.value("uniqueItems") == true && !s.checkKeyword("uniqueItems", value) {
			emit("must not have duplicate items")
		}
	}
	if text, ok := value.(string); ok {
		// TypeBox uses code points for JSON Schema string lengths, not UTF-16 units.
		s.argumentLimitErrors(float64(len([]rune(text))), "minLength", "maxLength", "characters", emit)
		if s.text("format") != "" && !s.checkKeyword("format", value) {
			emit(fmt.Sprintf("must match format \"%s\"", s.text("format")))
		}
		if s.text("pattern") != "" && !s.checkKeyword("pattern", value) {
			emit(fmt.Sprintf("must match pattern \"%s\"", s.text("pattern")))
		}
	}
	if number, ok := value.(float64); ok {
		for _, rule := range []struct {
			key, comparison string
			failed          func(float64) bool
		}{
			{"exclusiveMaximum", "<", func(limit float64) bool { return number >= limit }},
			{"exclusiveMinimum", ">", func(limit float64) bool { return number <= limit }},
			{"maximum", "<=", func(limit float64) bool { return number > limit }},
			{"minimum", ">=", func(limit float64) bool { return number < limit }},
		} {
			if limit, ok := s.value(rule.key).(float64); ok && rule.failed(limit) {
				emit("must be " + rule.comparison + " " + argumentNumberString(limit))
			}
		}
		if multiple, ok := s.value("multipleOf").(float64); ok && math.Mod(number, multiple) != 0 {
			emit("must be multiple of " + argumentNumberString(multiple))
		}
	}
	if ref := s.text("$ref"); ref != "" {
		if target := s.resolveArgumentRef(ref); target != nil {
			nested(target, value, path, pointer)
		}
	}
	if _, exists := s.fields["const"]; exists && !s.checkKeyword("const", value) {
		emit("must be equal to constant")
	}
	if _, exists := s.fields["enum"]; exists && !s.checkKeyword("enum", value) {
		emit("must be equal to one of the allowed values")
	}
	if condition := s.child("if"); condition != nil {
		branch := "else"
		if condition.check(value) {
			branch = "then"
		}
		// TypeBox collects the errors of a failing "then" branch in a context it drops; those of a failing "else" branch stay in the list.
		if child := s.child(branch); child != nil && ((branch == "then" && len(child.argumentErrorsAt(order, value, path, pointer)) > 0) || (branch == "else" && !nested(child, value, path, pointer))) {
			emit("must match \"" + branch + "\" schema")
		}
	}
	if child := s.child("not"); child != nil && child.check(value) {
		emit("must not be valid")
	}
	for _, child := range s.list("allOf") {
		nested(child, value, path, pointer)
	}
	for _, keyword := range []string{"anyOf", "oneOf"} {
		children := s.list(keyword)
		if children == nil {
			continue
		}
		count := 0
		var failures []string
		for _, child := range children {
			current := child.argumentErrorsAt(order, value, path, pointer)
			if len(current) == 0 {
				count++
			}
			failures = append(failures, current...)
		}
		if count == 0 || (keyword == "oneOf" && count != 1) {
			if count == 0 {
				lines = append(lines, failures...)
			}
			if keyword == "anyOf" {
				emit("must match a schema in anyOf")
			} else {
				emit("must match exactly one schema in oneOf")
			}
		}
	}
	return lines
}

// resolveArgumentRef 解析 $ref（仅支持 "#/path" 本地指针）：按 JSON
// Pointer 语义逐段下钻——~1→/、~0→~ 反转义，URL 解码，经 child() 走
// 惰性解析缓存；解不出来返回 nil（退化为不跟引用）。
func (s *argumentSchema) resolveArgumentRef(ref string) *argumentSchema {
	fragment, ok := strings.CutPrefix(ref, "#/")
	if !ok {
		return nil
	}
	fragment, err := url.PathUnescape(fragment)
	if err != nil {
		return nil
	}
	current := s.root
	for part := range strings.SplitSeq(fragment, "/") {
		if current == nil {
			return nil
		}
		current = current.child(strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~"))
	}
	return current
}

// checkKeyword 只按某个关键字校验值：临时构造单关键字 schema 走
// 编译校验器——错误遍历要用它判断"该报哪个关键字分支"。
func (s *argumentSchema) checkKeyword(keyword string, value any) bool {
	schema := &argumentSchema{doc: map[string]any{keyword: s.value(keyword)}}
	return schema.check(value)
}

// argumentLimitErrors 生成上/下限类错误（minItems/maxItems、
// minLength/maxLength、min/maxProperties）：TypeBox 的遍历顺序是
// 数组与字符串先上限后下限、properties 先下限后上限——报告顺序
// 也要与之一致。
func (s *argumentSchema) argumentLimitErrors(size float64, minimum, maximum, unit string, emit func(string)) {
	// Array and string upper bounds precede lower bounds in TypeBox's error traversal.
	order := []string{maximum, minimum}
	if unit == "properties" {
		order = []string{minimum, maximum}
	}
	for _, keyword := range order {
		limit, ok := s.value(keyword).(float64)
		if !ok {
			continue
		}
		if keyword == minimum && size < limit {
			emit("must not have fewer than " + argumentNumberString(limit) + " " + unit)
		}
		if keyword == maximum && size > limit {
			emit("must not have more than " + argumentNumberString(limit) + " " + unit)
		}
	}
}

// argumentPatternMatches 判定 pattern 是否命中：编译失败即不匹配。
func argumentPatternMatches(pattern, value string) bool {
	expression, err := regexp.Compile(pattern)
	return err == nil && expression.MatchString(value)
}

// argumentPath 拼接错误路径：根处 key 直接返回，否则 "path.key"。
func argumentPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// argumentKeySegment is a member name as it appears in Pi's error path: TypeBox appends the raw key to the instance path ("${instancePath}/${key}", unescaped) and validation.ts turns every "/" into ".".
//
// 成员名在错误路径里的呈现：TypeBox 原样拼 "${instancePath}/${key}"
// （不转义），validation.ts 再把每个 "/" 换成 "."——含斜杠的键名
// 在错误路径里会变成点多级，与上游逐字一致。
func argumentKeySegment(key string) string { return strings.ReplaceAll(key, "/", ".") }

// escapeArgumentPointer 是 JSON Pointer 转义：~→~0、/→~1。
func escapeArgumentPointer(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}
