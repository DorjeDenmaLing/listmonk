package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/knadh/listmonk/models"
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

// The templates API marks only the default visual template, beside listmonk's fields.
func TestDenmaMarkVisual(t *testing.T) {
	tpls := []models.Template{
		{Base: models.Base{ID: 1}, Name: "Sample Template", Type: models.TemplateTypeCampaign, IsDefault: true},
		{Base: models.Base{ID: 5}, Name: "DDL Default Template", Type: models.TemplateTypeCampaignVisual},
		{Base: models.Base{ID: 7}, Name: "DDL Welcome Template", Type: models.TemplateTypeCampaignVisual},
	}
	out := denmaMarkVisual(tpls, 5)
	if out[0].VisualDefault || !out[1].VisualDefault || out[2].VisualDefault {
		t.Fatalf("marked %v %v %v, want only template 5", out[0].VisualDefault, out[1].VisualDefault, out[2].VisualDefault)
	}
	b, err := json.Marshal(out[1])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"id":5`, `"name":"DDL Default Template"`, `"is_default":false`, `"visual_default":true`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("%s lacks %s", b, want)
		}
	}
	if denmaMarkVisual(tpls, 1)[0].VisualDefault {
		t.Fatal("an HTML template was marked the visual default")
	}
	for _, o := range denmaMarkVisual(tpls, 0) {
		if o.VisualDefault {
			t.Fatal("marked a template with no visual default")
		}
	}
}
