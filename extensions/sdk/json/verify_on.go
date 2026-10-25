//go:build pigjson_verify

// 中文导读：Go 扩展 SDK 的 JSON 设施——verify_on。
package json

// verifyTrusted: see verify_off.go. This build checks every trusted input and appender. pig additive (lw-json).
var verifyTrusted = true
