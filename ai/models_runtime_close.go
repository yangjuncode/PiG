// 中文导读：Models.Close——取消刷新并收尾。
//
// 取消所有进行中的刷新控制器并等待集合持有的操作结束；已完成的
// 刷新信号保持原有生命周期语义不变。
package ai

import "context"

// Close cancels active refreshes and joins collection-owned operations. Completed refresh signals keep their existing lifetime semantics.
func (m *Models) Close() {
	m.mu.Lock()
	for _, signal := range m.refreshControllers {
		signal.cancel(context.Canceled)
	}
	m.mu.Unlock()
	m.operations.Wait()
}

// beginProviderRefresh rejects a stale provider snapshot before allocating a new generation.
func (m *Models) beginProviderRefresh(ctx context.Context, provider *ModelsProvider) (uint64, *modelsRefreshSignal) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.providers[provider.ID] != provider {
		return 0, nil
	}
	generation := m.supersedeProviderRefreshLocked(provider.ID)
	signal := newModelsRefreshSignal(ctx)
	m.refreshControllers[provider.ID] = signal
	return generation, signal
}
