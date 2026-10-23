package main

import "testing"

func TestDenmaBulkFilter(t *testing.T) {
	for _, c := range []struct {
		status, query, want string
		bad                 bool
	}{
		{"", "", "", false},
		{"", "a = 1", "a = 1", false},
		{"blocklisted", "", "subscribers.status = 'blocklisted'", false},
		{"disabled", "a = 1 OR b = 2", "(a = 1 OR b = 2) AND subscribers.status = 'disabled'", false},
		{"enabled' OR '1'='1", "", "", true},
	} {
		q := c.query
		err := denmaWithSubStatus(c.status, &q)
		if (err != nil) != c.bad || (!c.bad && q != c.want) {
			t.Errorf("status %q, query %q: got %q (%v), want %q", c.status, c.query, q, err, c.want)
		}
	}
}
