package engine

import "sync"

// notify is a broadcast signal: Broadcast wakes every goroutine currently
// waiting on the channel returned by C.
type notify struct {
	mu sync.Mutex
	ch chan struct{}
}

// newNotify returns a notify with no waiters.
func newNotify() *notify {
	return &notify{ch: make(chan struct{})}
}

// C returns a channel that is closed by the next Broadcast.
func (n *notify) C() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.ch
}

// Broadcast wakes every current waiter.
func (n *notify) Broadcast() {
	n.mu.Lock()
	old := n.ch
	n.ch = make(chan struct{})
	n.mu.Unlock()
	close(old)
}
