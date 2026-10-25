// 中文导读：compat 标志的深拷贝（不走 JSON 往返）。
//
// cloneScalarCompat 用反射复制 compat 结构，保留标量值、空 map
// 的省略语义与显式空 fallback 列表——比 JSON 往返快且语义相同。
package ai

import (
	"reflect"

	"github.com/MichaelKinsy/PiG/internal/scalarjson"
)

// cloneScalarCompat preserves the serialized copy's scalar values, empty-map omission, and explicit empty fallback list without a JSON round trip.
func cloneScalarCompat(in *ModelCompat) (*ModelCompat, bool) {
	out := *in
	if !scalarjson.CloneFields(reflect.ValueOf(&out).Elem()) {
		return nil, false
	}
	return &out, true
}
