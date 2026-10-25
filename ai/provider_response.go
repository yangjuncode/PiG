// 中文导读：provider HTTP 响应的公共处理。
//
// 响应状态/错误体的统一读取与判定。
package ai

import (
	"context"
	"net/http"
	"strings"
)

// Ports packages/ai/src/utils/headers.ts
// headersToRecord mirrors Headers.entries, including its last set-cookie value.
func headersToRecord(headers http.Header) map[string]string {
	out := make(map[string]string, len(headers))
	for name, values := range headers {
		key := strings.ToLower(name)
		if len(values) == 0 {
			continue
		}
		if key == "set-cookie" {
			out[key] = values[len(values)-1]
		} else {
			out[key] = strings.Join(values, ", ")
		}
	}
	return out
}

func observeProviderResponse(ctx context.Context, opts StreamOptions, response *http.Response, model *Model) error {
	if opts.OnResponse == nil {
		return nil
	}
	return opts.OnResponse(ctx, ProviderResponse{Status: response.StatusCode, Headers: headersToRecord(response.Header)}, model)
}
