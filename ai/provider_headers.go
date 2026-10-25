// 中文导读：provider 请求头记录互转。
//
// http.Header 与上游 Record<string,string> 形状的互转
// （headers.ts 的 providerHeadersToRecord 等）。
package ai

import (
	"net/http"
	"strings"
)

func applyProviderHeaders(request *http.Request, headers ProviderHeaders) {
	for name, value := range headers {
		if strings.EqualFold(name, "host") {
			if value == nil {
				request.Host = ""
			} else {
				request.Host = *value
			}
			continue
		}
		if value == nil {
			request.Header.Del(name)
			continue
		}
		request.Header.Set(name, *value)
	}
}
