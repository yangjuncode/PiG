// 中文导读：消费者 goroutine 身份标记。
//
// StreamObservation 需要区分“跑消费者回调的 goroutine”与“只是
// 等待的 goroutine”——Result 只能靠 goroutine id 分辨。Current
// 需要解析栈头，成本不低，所以多次回调的 goroutine 只问一次
// （CurrentStreamOwner 缓存）。
package ai

import "github.com/MichaelKinsy/PiG/internal/goroutineid"

// currentGoroutineID identifies the calling goroutine (goroutineid.Current); tests count its calls. A stream observation
// records its consumer's identity because Result cannot otherwise tell the goroutine that runs the consumer callback,
// which yields it as `await stream.result()` does, from a goroutine that only waits.
//
// A call formats the goroutine's stack header, so a goroutine that runs many callbacks asks once (CurrentStreamOwner).
var currentGoroutineID = goroutineid.Current
