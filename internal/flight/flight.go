package flight

import (
	"context"
	"fmt"
	"sync"
)

// Group coalesces simultaneous requests for the same tile without retaining results.
type Group[K comparable, T any] struct {
	mu    sync.Mutex
	calls map[K]*call[T]
}

type call[T any] struct {
	done  chan struct{}
	value T
	err   error
}

func (g *Group[K, T]) DoContext(ctx context.Context, key K, fn func() (T, error)) (T, error, bool) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = make(map[K]*call[T])
	}
	if existing, ok := g.calls[key]; ok {
		g.mu.Unlock()
		select {
		case <-existing.done:
			return existing.value, existing.err, true
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err(), true
		}
	}
	c := &call[T]{done: make(chan struct{})}
	g.calls[key] = c
	g.mu.Unlock()

	var recovered any
	func() {
		defer func() {
			recovered = recover()
			if recovered != nil {
				c.err = fmt.Errorf("coalesced call panicked: %v", recovered)
			}
			close(c.done)
			g.mu.Lock()
			delete(g.calls, key)
			g.mu.Unlock()
		}()
		c.value, c.err = fn()
	}()
	if recovered != nil {
		panic(recovered)
	}
	return c.value, c.err, false
}
