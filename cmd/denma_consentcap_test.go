package main

import (
	"testing"

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
	"github.com/knadh/listmonk/models"
)

// An admin's save sends no opt-in to the subscriber it's for (by ID, or by
// address for a new one), in its center only, and only while it's made.
func TestDenmaOptinQuiet(t *testing.T) {
	ko := koanf.New(".")
	_ = ko.Load(confmap.Provider(map[string]any{"denma.center": "check-quiet"}, "."), nil)
	other := koanf.New(".")
	_ = other.Load(confmap.Provider(map[string]any{"denma.center": "check-quiet-other"}, "."), nil)
	a := &App{ko: ko}
	sent := 0
	send := denmaOptinQuiet(ko, func(models.Subscriber, []int) (int, error) { sent++; return 1, nil })
	sendOther := denmaOptinQuiet(other, func(models.Subscriber, []int) (int, error) { sent++; return 1, nil })
	sub := models.Subscriber{Base: models.Base{ID: 7}, Email: "Person@Example.org"}

	if n, _ := send(sub, nil); n != 1 || sent != 1 {
		t.Fatalf("not quiet: sent %d, want 1", sent)
	}
	done := a.denmaNoOptin(7, "")
	if n, _ := send(sub, nil); n != 0 || sent != 1 {
		t.Errorf("quiet by ID: sent %d, want none", sent)
	}
	if _, _ = sendOther(sub, nil); sent != 2 {
		t.Errorf("another center's subscriber 7 was kept quiet")
	}
	done()
	if _, _ = send(sub, nil); sent != 3 {
		t.Errorf("after the save: not sent")
	}

	done1 := a.denmaNoOptin(0, " person@example.ORG ")
	done2 := a.denmaNoOptin(0, "person@example.org")
	done1()
	if _, _ = send(sub, nil); sent != 3 {
		t.Errorf("quiet by address while a second save is under way: sent")
	}
	done2()
	if _, _ = send(sub, nil); sent != 4 {
		t.Errorf("after both saves: not sent")
	}
}
