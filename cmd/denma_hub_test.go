package main

import (
	"testing"

	"github.com/knadh/koanf/v2"
	"github.com/knadh/listmonk/internal/auth"
)

// A disabled account's session ends at its next request.
func TestDenmaSessionUser(t *testing.T) {
	ko := koanf.New(".") // a single install's
	if _, err := denmaSessionUser(ko, auth.User{Status: auth.UserStatusDisabled}, nil); err == nil {
		t.Error("a disabled account was let in")
	}
	if u, err := denmaSessionUser(ko, auth.User{Status: auth.UserStatusEnabled, Username: "x"}, nil); err != nil || u.Username != "x" {
		t.Errorf("an enabled account: %v", err)
	}
}
