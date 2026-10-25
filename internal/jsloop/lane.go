// 中文导读：JS 事件循环建模——lane；职责：Place is one call's reservation in a Lane. A nil *Place is no reservation: Wait, Release and Pass do nothing。
package jsloop

import (
	"sync"
	"sync/atomic"
)

// Lane keeps the calls of one destination in source order up to the point each issues its request. Pi runs the synchronous prefix of every call of `Promise.all(xs.map(f))` in source order, so f(a) issues its request before f(b). Go starts a goroutine per call and the scheduler may run the later one first, so the caller reserves each call's place in source order before it starts any goroutine, and a call issues its request only after every earlier place was released.
//
// The zero value is ready to use.
type Lane struct {
	mu   sync.Mutex
	tail *Place
}

// Place is one call's reservation in a Lane. A nil *Place is no reservation: Wait, Release and Pass do nothing.
//
// A place is one allocation and makes no channel unless a caller has to block on it: the next place's Wait finds an already released place with an atomic load. Each place holds the place it waits for only until it has waited or been released, so a long-lived lane keeps no chain of places.
type Place struct {
	// previous is the place this one waits for, nil once it waited, was released, or had none to wait for.
	previous atomic.Pointer[Place]
	released atomic.Bool
	mu       sync.Mutex
	// wake is made by the first caller that has to block on this place and closed by Release; guarded by mu.
	wake chan struct{}
}

// Reserve takes the next place in the lane. It must run in source order, before any of the calls runs.
func (l *Lane) Reserve() *Place {
	l.mu.Lock()
	p := After(l.tail)
	l.tail = p
	l.mu.Unlock()
	return p
}

// ReserveInto is Reserve for a caller that holds the place's storage, such as a field of a struct it allocates anyway: p must be the zero Place, and it is the new place.
func (l *Lane) ReserveInto(p *Place) *Place {
	l.mu.Lock()
	if l.tail != nil && !l.tail.released.Load() {
		p.previous.Store(l.tail)
	}
	l.tail = p
	l.mu.Unlock()
	return p
}

// After returns a place that waits for previous, which may be nil. It is Reserve for a caller that keeps the tail itself, for example one lane per key in a map it already guards: the call must run in source order, under the caller's own lock.
func After(previous *Place) *Place {
	p := new(Place)
	if previous != nil && !previous.released.Load() {
		p.previous.Store(previous)
	}
	return p
}

// Wait returns when every earlier place was released, provided each earlier call waited before it released (Wait then Release, or Pass). A place released without waiting hands on at once, and the place behind it waits for it, not for the places before it.
// 只等"前一个"位置——链上传递逐跳兑现：第 N 位释放后，
// 第 N+1 位的 Wait 回来即由它向第 N+2 位保证有序。等完
// CAS 掉 previous 引用，长 lane 不留整链。
func (p *Place) Wait() {
	if p == nil {
		return
	}
	previous := p.previous.Load()
	if previous == nil {
		return
	}
	previous.await()
	p.previous.CompareAndSwap(previous, nil)
}

// await returns when p was released.
// 先原子快查（已释放则零分配返回）；未释放才持锁建
// wake 通道——通道只在真有等待者时分配，Released 侧
// close 唤醒。锁内二次检查防与 Release 竞争丢唤醒。
func (p *Place) await() {
	if p.released.Load() {
		return
	}
	p.mu.Lock()
	if p.released.Load() {
		p.mu.Unlock()
		return
	}
	if p.wake == nil {
		p.wake = make(chan struct{})
	}
	wake := p.wake
	p.mu.Unlock()
	<-wake
}

// Release hands the place on, so the next place's Wait can return. It does not wait for the earlier places, and it is idempotent. It must run however the call ends. A released place waits for nothing: its own Wait returns at once.
func (p *Place) Release() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if !p.released.Load() {
		p.released.Store(true)
		if p.wake != nil {
			close(p.wake)
		}
	}
	p.mu.Unlock()
	p.previous.Store(nil)
}

// Pass waits for the earlier places and then hands this one on, so a call that ends without issuing its request cannot let a later call issue before an earlier one. It is idempotent.
func (p *Place) Pass() {
	p.Wait()
	p.Release()
}
