package flight

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

type signalingContext struct {
	context.Context
	doneCalled chan struct{}
	once       sync.Once
}

func (c *signalingContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.doneCalled) })
	return c.Context.Done()
}

func TestGroupCoalescesCalls(t *testing.T) {
	var group Group[string, int]
	var calls atomic.Int64
	started := make(chan struct{})
	release := make(chan struct{})
	type result struct {
		value  int
		err    error
		shared bool
	}
	leaderResult := make(chan result, 1)
	go func() {
		value, err, shared := group.DoContext(context.Background(), "tile", func() (int, error) {
			calls.Add(1)
			close(started)
			<-release
			return 42, nil
		})
		leaderResult <- result{value: value, err: err, shared: shared}
	}()
	<-started

	waiterResult := make(chan result, 1)
	waiterContext := &signalingContext{Context: context.Background(), doneCalled: make(chan struct{})}
	go func() {
		value, err, shared := group.DoContext(waiterContext, "tile", func() (int, error) {
			calls.Add(1)
			return 0, errors.New("waiter executed")
		})
		waiterResult <- result{value: value, err: err, shared: shared}
	}()
	<-waiterContext.doneCalled
	close(release)

	leader := <-leaderResult
	waiter := <-waiterResult
	if leader.err != nil || leader.shared || leader.value != 42 {
		t.Fatalf("unexpected leader result: %+v", leader)
	}
	if waiter.err != nil || !waiter.shared || waiter.value != 42 {
		t.Fatalf("unexpected waiter result: %+v", waiter)
	}
	if calls.Load() != 1 {
		t.Fatalf("function executed %d times", calls.Load())
	}
}

func TestGroupWaiterHonorsContext(t *testing.T) {
	var group Group[string, int]
	started := make(chan struct{})
	release := make(chan struct{})
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		_, _, _ = group.DoContext(context.Background(), "tile", func() (int, error) {
			close(started)
			<-release
			return 42, nil
		})
	}()
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	value, err, shared := group.DoContext(ctx, "tile", func() (int, error) {
		return 0, errors.New("waiter executed")
	})
	if value != 0 || !shared || !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected canceled waiter result: value=%d err=%v shared=%v", value, err, shared)
	}
	close(release)
	<-leaderDone
}

func BenchmarkGroupCoalescedWave(b *testing.B) {
	const waiters = 16
	var group Group[string, int]
	var calls atomic.Int64
	type result struct {
		value  int
		err    error
		shared bool
	}
	b.ReportAllocs()
	b.ReportMetric(waiters+1, "requests/wave")
	for b.Loop() {
		started := make(chan struct{})
		release := make(chan struct{})
		results := make(chan result, waiters+1)
		before := calls.Load()
		go func() {
			value, err, shared := group.DoContext(context.Background(), "tile", func() (int, error) {
				calls.Add(1)
				close(started)
				<-release
				return 42, nil
			})
			results <- result{value: value, err: err, shared: shared}
		}()
		<-started

		waiterContexts := make([]*signalingContext, 0, waiters)
		for range waiters {
			ctx := &signalingContext{Context: context.Background(), doneCalled: make(chan struct{})}
			waiterContexts = append(waiterContexts, ctx)
			go func() {
				value, err, shared := group.DoContext(ctx, "tile", func() (int, error) {
					calls.Add(1)
					return 0, errors.New("waiter executed")
				})
				results <- result{value: value, err: err, shared: shared}
			}()
		}
		for _, ctx := range waiterContexts {
			<-ctx.doneCalled
		}
		close(release)
		leaders := 0
		for range waiters + 1 {
			result := <-results
			if result.err != nil || result.value != 42 {
				b.Fatalf("unexpected coalesced result: %+v", result)
			}
			if !result.shared {
				leaders++
			}
		}
		if leaders != 1 || calls.Load() != before+1 {
			b.Fatalf("leaders=%d function calls=%d", leaders, calls.Load()-before)
		}
	}
}
