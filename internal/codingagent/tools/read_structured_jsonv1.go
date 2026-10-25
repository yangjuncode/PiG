//go:build !goexperiment.jsonv2

// 中文导读：read 工具结构化输出的 jsonv1 实现。
package tools

// encodedInvalidUTF8 is what encoding/json writes for a byte of ill-formed UTF-8 in a string: the legacy encoder escapes U+FFFD.
const encodedInvalidUTF8 = `\ufffd`
