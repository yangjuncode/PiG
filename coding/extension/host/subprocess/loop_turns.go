// 中文导读：子进程扩展宿主（D19）：扩展跑在独立进程，经 socket 帧协议与宿主通信；本目录是协议与宿主侧实现：loop_turns。
package subprocess

import (
	"strconv"
	"sync"

	"github.com/MichaelKinsy/PiG/extensions/sdk/json"
)

// pig additive (D19): Pi runs every extension on one event loop, so when a handler yields to it, the immediates and the due timers another extension queued run before that handler resumes, however the OS schedules the process (runner.ts:1096-1123). Node runtimes in separate processes keep that order only if the yielding process waits until every other one ran them: a stalled recipient otherwise reads the forwarded turn after the yielding handler answered, and runs its callbacks after the runner was invalidated. loopTurns holds each forwarded turn until every recipient took it, and then answers the sender, whose handler resumes only then.
type loopTurns struct {
	mu    sync.Mutex
	last  uint64
	turns map[uint64]*loopTurn
}

// loopTurn is one forwarded turn: its sender, the sender's own number for it, and the connections that have not taken it.
type loopTurn struct {
	sender  *Conn
	turn    uint64
	pending map[*Conn]struct{}
}

// forward numbers a turn of sender's for targets and returns the number and the NotifyLoopTurned frame that carries it. A target whose read loop ended is no recipient, and a turn with no recipient is taken at once.
// 判定"要不要等"：接收方全已关闭（pending 空）或发送方
// 自己也关了 → 不需要等待，立即回执 taken（发送方 handler
// 当场恢复）；否则登记 turn 让 settle/closed 收齐所有
// 接收方的取用回执再放行。
func (l *loopTurns) forward(sender *Conn, turn uint64, targets []*Conn) (uint64, encodedEnvelope) {
	l.mu.Lock()
	l.last++
	id := l.last
	pending := make(map[*Conn]struct{}, len(targets))
	for _, conn := range targets {
		if !conn.loopTurnsClosed {
			pending[conn] = struct{}{}
		}
	}
	waiting := len(pending) > 0 && !sender.loopTurnsClosed
	if waiting {
		if l.turns == nil {
			l.turns = make(map[uint64]*loopTurn)
		}
		l.turns[id] = &loopTurn{sender: sender, turn: turn, pending: pending}
	}
	l.mu.Unlock()
	if !waiting {
		sendLoopTurnTaken(sender, turn)
	}
	return id, loopTurnFrame(NotifyLoopTurned, id)
}

// take records conn's NotifyLoopTurnTaken.
func (l *loopTurns) take(conn *Conn, raw json.RawMessage) {
	var args LoopTurnArgs
	if json.Unmarshal(raw, &args) != nil {
		return
	}
	l.settle(conn, args.Turn)
}

// settle records that conn took turn id or can no longer take it, and answers the sender once no recipient remains.
func (l *loopTurns) settle(conn *Conn, id uint64) {
	l.mu.Lock()
	t := l.turns[id]
	if t != nil {
		delete(t.pending, conn)
		if len(t.pending) > 0 {
			t = nil
		} else {
			delete(l.turns, id)
		}
	}
	l.mu.Unlock()
	if t != nil {
		sendLoopTurnTaken(t.sender, t.turn)
	}
}

// closed records that conn's read loop ended: it takes no turn, and as a sender it waits for none.
// 双向清扫：conn 作为 sender 的未决 turn 直接删（它不会
// 再等回执）；作为接收方的 pending 名额核销，空 pending
// 的 turn 收齐即放行 sender。持锁外再发 taken，防在锁内
// 发帧阻塞。
func (l *loopTurns) closed(conn *Conn) {
	var taken []*loopTurn
	l.mu.Lock()
	conn.loopTurnsClosed = true
	for id, t := range l.turns {
		if t.sender == conn {
			delete(l.turns, id)
			continue
		}
		if _, ok := t.pending[conn]; !ok {
			continue
		}
		delete(t.pending, conn)
		if len(t.pending) == 0 {
			delete(l.turns, id)
			taken = append(taken, t)
		}
	}
	l.mu.Unlock()
	for _, t := range taken {
		sendLoopTurnTaken(t.sender, t.turn)
	}
}

// sendLoopTurnTaken tells sender that every recipient took its turn. A closed sender waits for nothing.
func sendLoopTurnTaken(sender *Conn, turn uint64) {
	_ = sender.sendEncoded(loopTurnFrame(NotifyLoopTurnTaken, turn))
}

// loopTurnFrame encodes the notify method with LoopTurnArgs numbered turn.
func loopTurnFrame(method string, turn uint64) encodedEnvelope {
	frame := make([]byte, 0, 96)
	frame = append(frame, `{"type":"notify","notify":{"method":"`...)
	frame = append(frame, method...)
	frame = append(frame, `","args":{"turn":`...)
	frame = strconv.AppendUint(frame, turn, 10)
	return append(frame, "}}}"...)
}
