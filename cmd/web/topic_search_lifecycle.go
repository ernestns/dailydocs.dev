package main

import (
	"context"
	"sync"
)

// Track admitted generation work independently from HTTP requests, which return
// as soon as a job is queued. Shutdown cancels providers but keeps SQLite open
// until their existing failure/partial-result cleanup has finished.
type topicSearchLifecycle struct {
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	stopping bool
	jobs     sync.WaitGroup
	done     chan struct{}
}

func newTopicSearchLifecycle() *topicSearchLifecycle {
	ctx, cancel := context.WithCancel(context.Background())
	return &topicSearchLifecycle{ctx: ctx, cancel: cancel, done: make(chan struct{})}
}

func (l *topicSearchLifecycle) admit() (context.Context, func(), bool) {
	if l == nil {
		return context.Background(), func() {}, true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopping {
		return nil, nil, false
	}
	l.jobs.Add(1)
	return l.ctx, l.jobs.Done, true
}

func (l *topicSearchLifecycle) stop() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopping {
		return
	}
	l.stopping = true
	l.cancel()
	// No new Add can race Wait after the admission gate closes.
	go func() {
		l.jobs.Wait()
		close(l.done)
	}()
}

func (l *topicSearchLifecycle) wait(ctx context.Context) error {
	if l == nil {
		return nil
	}
	l.stop()
	select {
	case <-l.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
