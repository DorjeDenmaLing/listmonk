package main

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/lib/pq"
)

// denma_is_import() knows the importer by its queries' text: both of them
// still have it, and no other subscriber insert does, so that an import can't
// set the center's attributes, in either mode.
func TestDenmaImportQueries(t *testing.T) {
	q := map[string]string{}
	for name, query := range readQueries(queryFilePath, fs) {
		q[name] = query.Query
	}
	marks := regexp.MustCompile(`strpos\(current_query\(\), '([^']+)'\)`).FindAllStringSubmatch(denmaFeaturesSQL, -1)
	if len(marks) != 2 {
		t.Fatalf("denma_is_import has %d query marks, want 2", len(marks))
	}
	for i, name := range []string{"upsert-subscriber", "upsert-blocklist-subscriber"} {
		if !strings.Contains(q[name], marks[i][1]) {
			t.Errorf("%s no longer has %q", name, marks[i][1])
		}
		if r, ok := denmaQueries[name]; ok && !strings.Contains(r, marks[i][1]) {
			t.Errorf("the fork's %s doesn't have %q", name, marks[i][1])
		}
	}
	for name, body := range q {
		if name == "upsert-subscriber" || name == "upsert-blocklist-subscriber" {
			continue
		}
		for _, m := range marks {
			if strings.Contains(body, m[1]) {
				t.Errorf("%s has the import's mark %q", name, m[1])
			}
		}
	}
}

// A center's update that deadlocks is tried again: the update's errors keep
// Postgres's, so the retry still sees the deadlock through them.
func TestDenmaFeaturesDeadlockRetried(t *testing.T) {
	calls := 0
	err := denmaRetryDeadlock(func() error {
		calls++
		if calls < 3 {
			return fmt.Errorf("installing the triggers: %w", &pq.Error{Code: "40P01"})
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("got %v after %d calls, want success on the third", err, calls)
	}
	calls = 0
	if err := denmaRetryDeadlock(func() error { calls++; return fmt.Errorf("x: %w", &pq.Error{Code: "42P01"}) }); err == nil || calls != 1 {
		t.Fatalf("another error: got %v after %d calls, want it at once", err, calls)
	}
}
