package main

import (
	"errors"
	"testing"
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
