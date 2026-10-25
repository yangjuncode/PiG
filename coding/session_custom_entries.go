// 中文导读：AppendCustomEntry：把扩展自有数据记为 custom 条目并发 entry_appended 事件——pi.appendEntry() 的 session 侧。
package coding

import "github.com/MichaelKinsy/PiG/agent"

// AppendCustomEntry records extension-owned data as a custom entry on the active branch and reports it as an
// `entry_appended` event, like the entries the core appends for its own state. It is the session behind upstream's
// `pi.appendEntry()`, for built-in extensions that reach the session through `ctx.sessionManager`.
//
// upstream: agent-session.ts (appendCustomEntry through the extension runner's appendEntry action)
func (s *Session) AppendCustomEntry(customType string, data any) (string, error) {
	id, err := s.inner.AppendCustomEntry(customType, data)
	if err != nil {
		return "", err
	}
	if entry, ok := s.inner.GetEntry(id); ok {
		s.emitEvent(agent.EntryAppendedEvent{Entry: entry.Raw()})
	}
	return id, nil
}
