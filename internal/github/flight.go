package github

import (
	"context"
	"sync"
	"time"
)

type flight[V any] struct {
	done chan struct{}
	val  V
	err  error
}

type flightGroup[K comparable, V any] struct {
	timeout time.Duration

	mu    sync.Mutex
	calls map[K]*flight[V]
}

func (g *flightGroup[K, V]) do(ctx context.Context, key K, fn func(context.Context) (V, error)) (V, error) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = make(map[K]*flight[V])
	}
	f, ok := g.calls[key]
	if !ok {
		f = &flight[V]{done: make(chan struct{})}
		g.calls[key] = f
		go g.run(context.WithoutCancel(ctx), key, f, fn)
	}
	g.mu.Unlock()

	select {
	case <-f.done:
		return f.val, f.err
	case <-ctx.Done():
		var zero V
		return zero, ctx.Err()
	}
}

func (g *flightGroup[K, V]) run(ctx context.Context, key K, f *flight[V], fn func(context.Context) (V, error)) {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	f.val, f.err = fn(ctx)
	g.mu.Lock()
	delete(g.calls, key)
	g.mu.Unlock()
	close(f.done)
}
