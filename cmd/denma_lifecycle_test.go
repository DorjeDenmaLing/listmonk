package main

import (
	"testing"
	"time"
)

// A center's stops under way are listed until they end, and a new App's
// scanning waits for those before it.
func TestDenmaStops(t *testing.T) {
	d := &denmaCenters{}
	if len(d.stopsOf("a")) != 0 {
		t.Fatal("stops with none begun")
	}
	end1 := d.beginStop("a")
	before := d.stopsOf("a")
	end2 := d.beginStop("a")
	if len(before) != 1 || len(d.stopsOf("a")) != 2 || len(d.stopsOf("b")) != 0 {
		t.Fatalf("stops: %d before, %d now", len(before), len(d.stopsOf("a")))
	}

	released := make(chan struct{})
	go func() { d.releaseScan(nil, before); close(released) }() // a nil App: nothing to release
	end2()
	if len(d.stopsOf("a")) != 1 {
		t.Fatal("the second stop is still listed")
	}
	end1()
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("the release didn't go on")
	}
	if len(d.stopsOf("a")) != 0 {
		t.Fatal("stops left")
	}
}
