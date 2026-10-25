// 中文导读：ObserveReplicatedState：扩展运行时复制的 Session 状态（活跃模型、思考等级、会话名、settings）发生未经扩展调用通报的变化时回调观察者。观察者必须立即返回且不得回调 Session/settings；多次调用返回的移除函数安全。
package coding

// ObserveReplicatedState registers observe to run after a change to Session state that an extension runtime replicates and that no extension call announced: the active model (set, cycled, restored with a replaced Session, or refreshed from the registry), the thinking level, the session name (set, or replaced with the Session) and the settings.
// Pi's extension context reads each of them live, so `ctx.model`, `pi.getThinkingLevel()`, `pi.getSessionName()` and `pi.getSettings()` return the new value at the next read (runner.ts:906, loader.ts:411-414, agent-session.ts); a runtime replicates the state, so its owner reports the change through this.
// observe runs on the goroutine that made the change, which may be the owner loop or hold a Session or settings lock: it must return at once and must not call back into the Session or its settings. It runs after the change, so a read from another goroutine sees the new state. The returned function removes the observer and can be called repeatedly.
func (s *Session) ObserveReplicatedState(observe func()) func() {
	stopSession := s.replicatedState.Add(observe)
	if settings := s.SettingsManager(); settings != nil {
		stopSettings := settings.ObserveChange(observe)
		return func() {
			stopSession()
			stopSettings()
		}
	}
	return stopSession
}

// notifyReplicatedState reports a change of the Session state an extension runtime replicates (ObserveReplicatedState). The settings report their own.
func (s *Session) notifyReplicatedState() { s.replicatedState.Notify() }
