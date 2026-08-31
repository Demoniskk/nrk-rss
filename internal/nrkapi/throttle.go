package nrkapi

import (
	"context"
	"sync"
	"time"
)

// throttle is a process-wide pause shared by every worker.
//
// NRK's rate limiter answers 429 with "Retry-After: 600" and stays angry while
// requests keep arriving. Backing off only the goroutine that got the 429 is
// therefore useless: the other workers keep hammering and keep the penalty
// alive. This makes one worker's 429 stop all of them.
type throttle struct {
	mu    sync.Mutex
	until time.Time
}

// penalize extends the pause so that no request is sent for at least d from
// now. An existing, longer pause is left in place.
func (t *throttle) penalize(d time.Duration) time.Time {
	if d <= 0 {
		return time.Time{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	deadline := time.Now().Add(d)
	if deadline.After(t.until) {
		t.until = deadline
	}
	return t.until
}

// deadline reports the current pause deadline, if any.
func (t *throttle) deadline() time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.until
}

// wait blocks until the pause has elapsed, or the context is done.
func (t *throttle) wait(ctx context.Context) error {
	for {
		until := t.deadline()
		d := time.Until(until)
		if d <= 0 {
			return nil
		}

		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
			// Re-check: another worker may have extended the pause while this
			// one was sleeping.
		}
	}
}
