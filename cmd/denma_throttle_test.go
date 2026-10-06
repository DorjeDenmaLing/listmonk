package main

import (
	"fmt"
	"testing"
	"time"
)

func TestDenmaThrottle(t *testing.T) {
	th := newDenmaThrottle(3, 50*time.Millisecond)
	for i := 0; i < 2; i++ {
		th.add("a")
	}
	if w := th.wait("a"); w != 0 {
		t.Fatalf("under the limit: wait %s, want 0", w)
	}
	th.add("a")
	if w := th.wait("a"); w <= 0 || w > 50*time.Millisecond {
		t.Fatalf("at the limit: wait %s, want (0, 50ms]", w)
	}
	if w := th.wait("b"); w != 0 {
		t.Fatalf("another key: wait %s, want 0", w)
	}

	time.Sleep(60 * time.Millisecond)
	if w := th.wait("a"); w != 0 {
		t.Fatalf("after the window: wait %s, want 0", w)
	}

	for i := 0; i < 5; i++ {
		th.add("c")
	}
	th.clear("c")
	if w := th.wait("c"); w != 0 {
		t.Fatalf("cleared: wait %s, want 0", w)
	}
}

func TestDenmaThrottleDropsExpiredKeys(t *testing.T) {
	th := newDenmaThrottle(3, time.Millisecond)
	for i := 0; i < denmaThrottleKeys; i++ {
		th.add(fmt.Sprint(i))
	}
	time.Sleep(5 * time.Millisecond)
	th.add("new")
	if n := len(th.hits); n != 1 {
		t.Fatalf("%d keys kept, want 1", n)
	}
}
