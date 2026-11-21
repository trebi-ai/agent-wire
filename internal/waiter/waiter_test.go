package waiter

import (
	"errors"
	"testing"
	"time"
)

func TestTableDeliverTwice(t *testing.T) {
	var tb Table[string]
	ch, err := tb.Expect("1")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan [2]bool)
	go func() { done <- [2]bool{tb.Deliver("1", "a"), tb.Deliver("1", "b")} }()
	select {
	case got := <-done:
		if !got[0] || got[1] {
			t.Fatalf("Deliver = %v, want [true false]", got)
		}
	case <-time.After(time.Second):
		t.Fatal("Deliver blocked")
	}
	if v := <-ch; v != "a" {
		t.Fatalf("got %q, want a", v)
	}
	if tb.Len() != 0 {
		t.Fatalf("Len = %d, want 0", tb.Len())
	}
}

func TestTableForget(t *testing.T) {
	var tb Table[int]
	_, _ = tb.Expect("1")
	tb.Forget("1")
	if tb.Deliver("1", 1) {
		t.Fatal("Deliver found a forgotten waiter")
	}
}

func TestTableClose(t *testing.T) {
	var tb Table[int]
	ch, _ := tb.Expect("1")
	boom := errors.New("boom")
	if !tb.Close(boom) {
		t.Fatal("first Close = false")
	}
	if _, ok := <-ch; ok {
		t.Fatal("channel not closed")
	}
	if _, err := tb.Expect("2"); !errors.Is(err, boom) {
		t.Fatalf("Expect err = %v, want boom", err)
	}
	if tb.Err() != boom {
		t.Fatalf("Err = %v", tb.Err())
	}
	if tb.Close(errors.New("other")) {
		t.Fatal("second Close = true")
	}
	tb.Forget("1")
}
