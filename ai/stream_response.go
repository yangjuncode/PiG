// 中文导读：assistant 流构建器的响应运行驱动。
//
// runResponse 在栈底标记（stackprime）保护下运行 provider 的
// 响应函数，处理 abort 与结果装配。
package ai

import "github.com/MichaelKinsy/PiG/internal/stackprime"

func (builder *assistantStreamBuilder) runResponse(run func() error) {
	stackprime.Prime()
	if builder.abort != nil {
		defer builder.abort()
	}
	if err := run(); err != nil {
		reason := StopReasonError
		if builder.ctx.Err() != nil {
			reason = StopReasonAborted
		}
		_ = builder.responseTurn(func() error {
			builder.failUnfinished(reason, err)
			return nil
		})
	}
}
