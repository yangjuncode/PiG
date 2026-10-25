// 中文导读：process.nextTick 的建模。
//
// postTick 把回调排进 nextTick 队列并启动 drain——nextTick 先于
// promise 反应执行（Node 语义），是 continuation executor 时序的
// 一部分。
package ai

// postTick queues a process.nextTick callback and runs the drain it starts.
func (executor *continuationExecutor) postTick(reaction func()) {
	if drain := executor.postTickDeferred(reaction); drain != nil {
		drain()
	}
}
