package main

import (
	"net/http"
	"testing"

	"github.com/labstack/echo/v4"
)

// An IPv6 client counts as its /64; an IPv4 one, and a mapped one, as itself.
func TestDenmaIPKey(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.7":                   "203.0.113.7",
		"::ffff:203.0.113.7":            "203.0.113.7",
		"2001:db8:1:2:3:4:5:6":          "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff:ffff:ffff:1": "2001:db8:1:2::/64",
		"2001:db8:1:3::1":               "2001:db8:1:3::/64",
		"fe80::1%eth0":                  "fe80::/64",
		"not an address":                "not an address",
	} {
		if got := denmaIPKey(in); got != want {
			t.Errorf("denmaIPKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// Wrong current passwords are limited as sign-in's are, and a right one clears them.
func TestDenmaCurrentPassword(t *testing.T) {
	const key = "person:test-current-password"
	denmaPasswordByUser.clear(key)
	defer denmaPasswordByUser.clear(key)
	checks := 0
	check := func(right bool) func() (bool, error) {
		return func() (bool, error) { checks++; return right, nil }
	}

	for i := 0; i < 9; i++ {
		if ok, err := denmaCurrentPassword(key, check(false)); ok || err != nil {
			t.Fatalf("wrong password %d: %v, %v", i+1, ok, err)
		}
	}
	if ok, err := denmaCurrentPassword(key, check(true)); !ok || err != nil {
		t.Fatalf("a right password under the limit: %v, %v", ok, err)
	}
	for i := 0; i < 10; i++ {
		_, _ = denmaCurrentPassword(key, check(false))
	}
	before := checks
	ok, err := denmaCurrentPassword(key, check(true))
	if ok || checks != before {
		t.Fatalf("past the limit the password was checked: %v (%d checks)", ok, checks-before)
	}
	if he, isHTTP := err.(*echo.HTTPError); !isHTTP || he.Code != http.StatusTooManyRequests {
		t.Fatalf("past the limit: %v, want 429", err)
	}
}
