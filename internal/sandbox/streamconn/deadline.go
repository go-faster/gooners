package streamconn

import (
	"sync"
	"time"
)

// deadline turns a mutable point in time into a channel that closes once that
// time has passed, so a blocked select can react to a deadline set (or moved)
// concurrently by another goroutine.
type deadline struct {
	mu     sync.Mutex
	timer  *time.Timer
	cancel chan struct{} // closed once the current deadline has elapsed
}

func newDeadline() *deadline {
	return &deadline{cancel: make(chan struct{})}
}

// set updates the deadline. A zero t disables it.
func (d *deadline) set(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.timer != nil {
		if !d.timer.Stop() {
			<-d.cancel // wait for the in-flight callback to finish closing cancel
		}
		d.timer = nil
	}

	closed := isClosed(d.cancel)
	switch {
	case t.IsZero():
		// No deadline: make sure waiters observe an open channel.
		if closed {
			d.cancel = make(chan struct{})
		}
	case time.Until(t) <= 0:
		// Already past: signal immediately.
		if !closed {
			close(d.cancel)
		}
	default:
		if closed {
			d.cancel = make(chan struct{})
		}
		cancel := d.cancel
		d.timer = time.AfterFunc(time.Until(t), func() {
			close(cancel)
		})
	}
}

// wait returns the channel that closes when the deadline elapses. The
// returned channel never fires if no deadline is set.
func (d *deadline) wait() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cancel
}

func isClosed(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}
