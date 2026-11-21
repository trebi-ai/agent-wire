// Package waiter matches replies to the requests that wait for them.
package waiter

import "sync"

// Table maps a request id to the channel of its waiter. The zero value is
// ready to use.
type Table[V any] struct {
	mu  sync.Mutex
	m   map[string]chan V
	err error
}

// Expect adds a waiter for id. After Close it returns the close error.
func (t *Table[V]) Expect(id string) (<-chan V, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err != nil {
		return nil, t.err
	}
	if t.m == nil {
		t.m = map[string]chan V{}
	}
	ch := make(chan V, 1)
	t.m[id] = ch
	return ch, nil
}

// Deliver removes the waiter of id and sends v to it. It never blocks. It
// reports false for an unknown id, so a second reply is dropped.
func (t *Table[V]) Deliver(id string, v V) bool {
	t.mu.Lock()
	ch, ok := t.m[id]
	delete(t.m, id)
	t.mu.Unlock()
	if ok {
		ch <- v
	}
	return ok
}

// Forget removes the waiter of id. A racing Deliver still finds a valid
// channel or no waiter.
func (t *Table[V]) Forget(id string) {
	t.mu.Lock()
	delete(t.m, id)
	t.mu.Unlock()
}

// Close stores err, removes every waiter, and closes their channels. It
// reports false when the table was already closed.
func (t *Table[V]) Close(err error) bool {
	t.mu.Lock()
	if t.err != nil {
		t.mu.Unlock()
		return false
	}
	t.err = err
	waiters := t.m
	t.m = nil
	t.mu.Unlock()
	for _, ch := range waiters {
		close(ch)
	}
	return true
}

// Err returns the close error, or nil while the table is open.
func (t *Table[V]) Err() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

// Len is the number of waiters.
func (t *Table[V]) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.m)
}
