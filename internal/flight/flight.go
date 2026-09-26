// Package flight collapses concurrent same-key calls into one execution.
package flight

import "sync"

// Group runs one call per key at a time. Callers that arrive while a call is
// in flight share its result.
type Group[K comparable, T any] struct {
	mu    sync.Mutex
	calls map[K]*call[T]
}

type call[T any] struct {
	wg  sync.WaitGroup
	val T
	err error
}

// Do runs fn for key, or waits for a call with the same key. The fn of the
// first caller runs; every caller gets its result.
func (g *Group[K, T]) Do(key K, fn func() (T, error)) (T, error) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = map[K]*call[T]{}
	}
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err
	}
	c := &call[T]{}
	c.wg.Add(1)
	g.calls[key] = c
	g.mu.Unlock()

	c.val, c.err = fn()
	c.wg.Done()

	g.mu.Lock()
	delete(g.calls, key)
	g.mu.Unlock()
	return c.val, c.err
}
