package main

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// The process's log has no addresses, whoever wrote the line: each is its
// reference, the start of its hash as the database has it.
func TestDenmaRedactLog(t *testing.T) {
	var b bytes.Buffer
	l := log.New(denmaRedactLog{&b}, "", 0)
	l.Printf("denma: blocklisting Someone@Example.org: hard bounce")
	l.Printf("error: pq: alice@b.example unsubscribed from this center")
	l.Printf("no address here, but a @mention")
	out := b.String()
	if strings.Contains(out, "@Example.org") || strings.Contains(out, "alice@") {
		t.Errorf("an address was logged: %s", out)
	}
	if ref := denmaAddrRef("someone@example.org"); !strings.Contains(out, "blocklisting "+ref+": hard bounce") || ref != denmaAddrRef(" SOMEONE@example.org ") {
		t.Errorf("the reference: %s in %s", ref, out)
	}
	if !strings.Contains(out, "a @mention") {
		t.Errorf("text without an address changed: %s", out)
	}
	if got := denmaRedactURL("/admin/login/twofa?token=abc123&next=%2Fadmin"); strings.Contains(got, "abc123") || !strings.Contains(got, "next=") {
		t.Errorf("redirect: %s", got)
	}
}
