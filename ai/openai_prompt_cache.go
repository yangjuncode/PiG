// 中文导读：OpenAI prompt cache 的 key 计算（上游 64 Unicode scalar 限制且不劈开 UTF-8 序列）。
package ai

import "unicode/utf8"

const openAIPromptCacheKeyMaxLength = 64

// ClampOpenAIPromptCacheKey clamps an OpenAI prompt cache/session key to
// upstream's 64-Unicode-scalar limit without splitting UTF-8 sequences.
func ClampOpenAIPromptCacheKey(key string) string {
	if utf8.RuneCountInString(key) <= openAIPromptCacheKeyMaxLength {
		return key
	}
	i := 0
	for pos := range key {
		if i == openAIPromptCacheKeyMaxLength {
			return key[:pos]
		}
		i++
	}
	return key
}
