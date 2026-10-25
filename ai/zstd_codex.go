//go:build !pig_strip_node_extensions

// 中文导读：Codex SSE 请求体的 zstd 压缩。
//
// 按 Pi 的 REQUEST_COMPRESSION_ZSTD_LEVEL=3 压缩
// （openai-codex-responses.ts compressRequestBodyZstd）；编码器
// 首次使用时构建并共享。
package ai

import (
	"sync"

	"github.com/klauspost/compress/zstd"
)

// codexZstdEncoder compresses Codex SSE request bodies at zstd level 3, the level Pi's REQUEST_COMPRESSION_ZSTD_LEVEL names (openai-codex-responses.ts compressRequestBodyZstd). It is built on first use and shared:
// one encoder instance serves EncodeAll, so concurrent requests wait for it instead of each holding an encoder's buffers. zstd level 3 uses a 2 MiB window (windowLog 21); the library's default 8 MiB window made the
// encoder retain 18 MB after its first call for the same frame size, and 2 MiB with the lower-memory buffers retains 3.7 MB.
var codexZstdEncoder = sync.OnceValue(func() *zstd.Encoder {
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(1<<21), zstd.WithLowerEncoderMem(true))
	if err != nil {
		panic(err) // the options are constants
	}
	return encoder
})

// compressCodexRequestBody is the zstd frame of body that the Codex backend decodes for Content-Encoding: zstd.
func compressCodexRequestBody(body []byte) []byte {
	return codexZstdEncoder().EncodeAll(body, make([]byte, 0, len(body)/4+64))
}
