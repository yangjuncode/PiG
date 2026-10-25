// 中文导读：请求体的原始拼接出口。
//
// marshalRequestBody 的最终组装：已编码片段与缓存 splice 的
// 字节拼装（jsonstringify）。
package ai

import (
	"encoding/json"

	"github.com/MichaelKinsy/PiG/internal/jsonstringify"
)

// rawRequestBody is the body of a replacement a payload hook returned as JSON text. Pi's hook returns a parsed value and the SDK JSON.stringify's it, so
// the text is written as JSON.parse then JSON.stringify gives it: escapes decoded, numbers in JavaScript's spelling, a repeated key at its first position with
// its last value.
func rawRequestBody(body any) ([]byte, bool, error) {
	raw, ok := body.(json.RawMessage)
	if !ok {
		return nil, false, nil
	}
	canonical, err := jsonstringify.Canonicalize(raw)
	return canonical, true, err
}
