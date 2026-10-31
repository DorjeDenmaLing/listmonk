package models

import (
	"errors"
	"testing"
	"time"
)

func TestDenmaTplBufferTime(t *testing.T) {
	var b DenmaTplBuffer
	if _, err := b.Write(nil); err != nil {
		t.Fatalf("the first write: %v", err)
	}
	if b.start.IsZero() {
		t.Fatal("an empty write didn't start the clock")
	}

	b.start = time.Now().Add(-DenmaTplTimeMax + time.Second)
	if _, err := b.Write(nil); err != nil {
		t.Errorf("within the time: %v", err)
	}

	b.start = time.Now().Add(-DenmaTplTimeMax - time.Second)
	if _, err := b.Write(nil); !errors.Is(err, ErrDenmaTplTime) {
		t.Errorf("past the time, writing nothing: %v", err)
	}
	if _, err := b.Write([]byte("x")); !errors.Is(err, ErrDenmaTplTime) {
		t.Errorf("past the time, writing: %v", err)
	}

	b.Reset()
	if _, err := b.Write([]byte("x")); err != nil {
		t.Errorf("after Reset: %v", err)
	}
}

func TestDenmaTplBufferPauses(t *testing.T) {
	write := func(b *DenmaTplBuffer) time.Duration {
		t.Helper()
		start := time.Now()
		if _, err := b.Write(nil); err != nil {
			t.Fatal(err)
		}
		return time.Since(start)
	}

	var b DenmaTplBuffer
	write(&b)
	if d := write(&b); d >= denmaTplPauseTime {
		t.Errorf("a new render paused for %v", d)
	}

	b.start = time.Now().Add(-time.Second)
	if d := write(&b); d < denmaTplPauseTime {
		t.Errorf("a long render paused for only %v", d)
	}
	if d := write(&b); d >= denmaTplPauseTime {
		t.Errorf("paused again straight away: %v", d)
	}
}
