/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package duplex

import (
	"context"
	"sync"
)

// inflightTracker counts in-flight requests and lets a caller wait for them to
// drain. Its zero value is ready to use.
type inflightTracker struct {
	mu    sync.Mutex
	count int

	// idle is created by the first waiter and closed when count reaches zero, so
	// waiters can select on it alongside their context.
	idle chan struct{}
}

func (t *inflightTracker) add() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.count++
}

func (t *inflightTracker) done() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.count--
	if t.count == 0 && t.idle != nil {
		close(t.idle)
		t.idle = nil
	}
}

// wait blocks until no requests are in flight, returning nil, or until ctx is
// done, returning ctx.Err(). A ctx without a deadline waits indefinitely for
// the count to reach zero.
func (t *inflightTracker) wait(ctx context.Context) error {
	t.mu.Lock()
	if t.count == 0 {
		t.mu.Unlock()
		return nil
	}

	if t.idle == nil {
		t.idle = make(chan struct{})
	}
	idle := t.idle
	t.mu.Unlock()

	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
