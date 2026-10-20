package main

import (
	"regexp"
	"strings"
	"testing"
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
