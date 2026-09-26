package flight

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGroupSharesOneCall(t *testing.T) {
	var runs atomic.Int64
	release := make(chan struct{})
	var g Group[string, int]
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			val, err := g.Do("k", func() (int, error) {
				runs.Add(1)
				time.Sleep(50 * time.Millisecond)
				<-release
				return 7, nil
			})
			if err != nil || val != 7 {
				t.Errorf("Do = %d, %v", val, err)
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if got := runs.Load(); got != 1 {
		t.Fatalf("fn ran %d times, want 1", got)
	}
}

func TestGroupSeparateKeysAndErrors(t *testing.T) {
	var g Group[string, int]
	if _, err := g.Do("a", func() (int, error) { return 0, errors.New("boom") }); err == nil {
		t.Fatal("want the error back")
	}
	// A failed call is not cached: the next caller runs again.
	if _, err := g.Do("a", func() (int, error) { return 5, nil }); err != nil {
		t.Fatal(err)
	}
	if v, _ := g.Do("b", func() (int, error) { return 6, nil }); v != 6 {
		t.Fatalf("v = %d", v)
	}
}
