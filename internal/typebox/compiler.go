// Package typebox ports the parts of TypeBox 1.3.27 Value that PiG needs over JSON Schema data: Check, Errors, Clean,
// Default and Clone, with the format registry and the keyword spellings TypeBox accepts.
package typebox

import "maps"

// CompilerSchema translates the positional items and dependencies TypeBox accepts alongside current JSON Schema
// keywords into their draft-2020 spellings for the validator, without changing the schema used for conversion or error
// traversal.
//
// 给严格 draft-2020 编译器用的"方言翻译"：TypeBox schema 里可能带老式
// 写法（items 元组数组、dependencies），校验器不认，于是在编译副本上
// 把它们改写成 prefixItems / dependentRequired / dependentSchemas 的
// allOf 组合。只影响 validator() 用的那份，convert/coerce/错误遍历
// 仍在原始 schema 树上走。
func CompilerSchema(value any) any {
	original, ok := value.(map[string]any)
	if !ok {
		return value
	}
	// maps.Clone 浅拷贝本层：递归只发生在 schema 关键字里，
	// 非 schema 字段（enum/const/default 等数据值）引用共享——
	// 翻译产物的所有权只到 schema 结构层。
	schema := maps.Clone(original)
	// 三类容器的递归：map 型子 schema（properties 等）、数组
	// 型（allOf 等）、单子 schema（additionalProperties 等）。
	// 覆盖 TypeBox/2020 全部可嵌套位置。
	for _, keyword := range []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"} {
		if children, ok := schema[keyword].(map[string]any); ok {
			translated := make(map[string]any, len(children))
			for key, child := range children {
				translated[key] = CompilerSchema(child)
			}
			schema[keyword] = translated
		}
	}
	for _, keyword := range []string{"allOf", "anyOf", "oneOf", "prefixItems"} {
		if children, ok := schema[keyword].([]any); ok {
			translated := make([]any, len(children))
			for i, child := range children {
				translated[i] = CompilerSchema(child)
			}
			schema[keyword] = translated
		}
	}
	for _, keyword := range []string{"additionalProperties", "propertyNames", "contains", "if", "then", "else", "not", "unevaluatedProperties", "unevaluatedItems"} {
		if child, ok := schema[keyword]; ok {
			schema[keyword] = CompilerSchema(child)
		}
	}
	// 老写法一：items 元组（数组）→ draft-2020 的
	// prefixItems + items，包进一条 allOf 追加在末尾。
	// 不直接改写 items 是因为 prefixItems 不允许空数组
	// （TypeBox 允许），要经 allOf 间接挂载。
	if tuple, ok := schema["items"].([]any); ok {
		translated := make([]any, len(tuple))
		for i, child := range tuple {
			translated[i] = CompilerSchema(child)
		}
		tupleSchema := map[string]any{}
		// Empty positional items is permitted by TypeBox, while prefixItems must be nonempty.
		if len(translated) > 0 {
			tupleSchema["prefixItems"] = translated
		}
		if additional, exists := schema["additionalItems"]; exists {
			tupleSchema["items"] = CompilerSchema(additional)
		}
		allOf, _ := schema["allOf"].([]any)
		schema["allOf"] = append(allOf, tupleSchema)
		delete(schema, "items")
		delete(schema, "additionalItems")
	} else if item, exists := schema["items"]; exists {
		schema["items"] = CompilerSchema(item)
	}
	// 老写法二：dependencies（子值数组 → dependentRequired；
	// 子值 schema → dependentSchemas）→ 逐条追加进 allOf。
	if dependencies, ok := schema["dependencies"].(map[string]any); ok {
		allOf, _ := schema["allOf"].([]any)
		for property, child := range dependencies {
			keyword := "dependentSchemas"
			if _, ok := child.([]any); ok {
				keyword = "dependentRequired"
			} else {
				child = CompilerSchema(child)
			}
			allOf = append(allOf, map[string]any{keyword: map[string]any{property: child}})
		}
		if len(allOf) > 0 {
			schema["allOf"] = allOf
		}
		delete(schema, "dependencies")
	}
	return schema
}
