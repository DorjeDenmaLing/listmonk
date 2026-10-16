package main

import (
	"errors"
	"testing"
	"time"
)

// Transactional e-mails claim the hub's daily limit short of its reserve,
// all or none, leaving the reserve to opt-ins and password resets.
func TestDenmaClaimDaily(t *testing.T) {
	defer denmaDaily.SetLimit(0, 0)
	denmaDaily.SetLimit(10, 50) // 5 for campaigns, automations and transactional e-mails
	if claimed, err := denmaClaimDaily(6); !errors.Is(err, errDenmaDaily) || claimed {
		t.Errorf("6 of 5: claimed %v, %v", claimed, err)
	}
	if claimed, err := denmaClaimDaily(5); err != nil || !claimed {
		t.Errorf("5 of 5: claimed %v, %v", claimed, err)
	}
	if _, err := denmaClaimDaily(1); !errors.Is(err, errDenmaDaily) {
		t.Errorf("one more: %v", err)
	}
	denmaDaily.Release(5)
	denmaDaily.SetLimit(0, 0)
	if claimed, err := denmaClaimDaily(1000); err != nil || claimed {
		t.Errorf("without a limit: claimed %v, %v", claimed, err)
	}
}

// A message waiting for its turn at the send rate gives up as soon as it's
// stopped (a shutdown), however far off its turn is; one not stopped waits.
func TestDenmaPacerStops(t *testing.T) {
	p := &denmaPacer{rate: 1}
	for range 20 { // turns taken for the next 20 seconds
		p.waitUnless(func() bool { return true })
	}
	start, stop := time.Now(), time.Now().Add(100*time.Millisecond)
	if p.waitUnless(func() bool { return time.Now().After(stop) }) {
		t.Fatal("a message 20 s from its turn went")
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("gave up after %v, want about 100ms", d)
	}
	q := &denmaPacer{rate: 10}
	if !q.waitUnless(func() bool { return false }) {
		t.Fatal("a message with its turn now didn't go")
	}
}
