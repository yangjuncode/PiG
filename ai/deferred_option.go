// 中文导读：SimpleStreamOptions.deferred（types.ts 移植）。
//
// 简单流的延迟选项：允许把简单流的某些决策推迟到流建立时，
// 保持与上游同名字段相同语义。
package ai

// Ports packages/ai/src/types.ts (SimpleStreamOptions.deferred).

import (
	"encoding/json"
	"fmt"
)

// DeferredOption is a deferred-generation boolean or an object with an optional window.
type DeferredOption struct {
	Object  bool
	Enabled bool
	Window  string
}

// MarshalJSON emits the upstream boolean or object form.
func (option DeferredOption) MarshalJSON() ([]byte, error) {
	if !option.Object {
		return json.Marshal(option.Enabled)
	}
	return json.Marshal(struct {
		Window string `json:"window,omitempty"`
	}{Window: option.Window})
}

// UnmarshalJSON decodes either deferred-generation option form.
func (option *DeferredOption) UnmarshalJSON(data []byte) error {
	var enabled bool
	if err := json.Unmarshal(data, &enabled); err == nil {
		*option = DeferredOption{Enabled: enabled}
		return nil
	}
	var object struct {
		Window string `json:"window"`
	}
	if err := json.Unmarshal(data, &object); err != nil {
		return fmt.Errorf("deferred option: %w", err)
	}
	*option = DeferredOption{Object: true, Window: object.Window}
	return nil
}
