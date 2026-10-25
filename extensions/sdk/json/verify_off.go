//go:build !pigjson_verify

// 中文导读：Go 扩展 SDK 的 JSON 设施——verify_off。
package json

// verifyTrusted makes UnmarshalTrusted validate its input and the encoder check every StringifyAppender against the compaction it skips. The
// package's own tests turn it on; build with -tags pigjson_verify to check every caller's tests. pig additive (lw-json).
var verifyTrusted = false
