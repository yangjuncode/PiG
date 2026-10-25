// 中文导读：Piglet 可裁掉的 provider API（strip.apis）。
//
// pig additive (D92)：Stock PiG 不裁任何 API。Piglet Binary 用
// pig_strip_<api> 构建标签把对应实现整体编出，Stock PiG 运行的
// Piglet 则在运行时禁用。被裁 API 的模型不出现在 OfferedModels，
// 为其构建 provider 会失败。
package ai

// StrippableAPIs lists the APIs a Piglet can strip (strip.apis). A Piglet
// Binary compiles each one's implementation out with the build tag
// pig_strip_<api> (see internal/pigstrip.Tag), and a Piglet that Stock PiG
// runs disables it at runtime. A stripped API's models are not offered
// (OfferedModels), and building a provider for one of them fails.
// pig additive (D92): Stock PiG strips no API.
func StrippableAPIs() []API {
	return []API{APIBedrockConverseStream, APIGoogleVertex, APIMistralConversations}
}
