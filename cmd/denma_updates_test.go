package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDenmaBuildCommit(t *testing.T) {
	for build, want := range map[string]string{
		"v6.0.0 (ddl 9b35d629 2026-11-01T23:50:00Z)": "9b35d629",
		"v6.0.0 (dev 9b35d62 abc)":                   "9b35d62",
		"v6.0.0 (#1234abc 2026-11-01)":               "1234abc",
		"v6.0.0":                                     "",
	} {
		got := ""
		if m := denmaBuildCommit.FindStringSubmatch(build); m != nil {
			got = m[1]
		}
		if got != want {
			t.Errorf("%q: got %q", build, got)
		}
	}
}

func TestDenmaFindUpdate(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	checkFile := filepath.Join(dir, "check")
	asked := func() string {
		b, _ := os.ReadFile(checkFile)
		return string(b)
	}

	found, checking := denmaFindUpdate(dir, "aaaaaaaa", time.Hour, now)
	if !checking || asked() != "aaaaaaaa\n" || found.Running != "aaaaaaaa" || found.Status != "" {
		t.Fatalf("with nothing found yet, a check is asked for: %+v %v %q", found, checking, asked())
	}
	if _, checking := denmaFindUpdate(dir, "aaaaaaaa", time.Hour, now); !checking {
		t.Fatal("while it waits, it's still checking")
	}
	if found, checking := denmaFindUpdate(dir, "aaaaaaaa", time.Hour, now.Add(3*time.Minute)); checking || found.Error == "" || asked() != "" {
		t.Fatalf("a check nobody takes says so, and is withdrawn: %+v %v", found, checking)
	}

	avail := `{"running":"aaaaaaaa","head":"` + strings.Repeat("c", 40) + `","status":"ahead","ahead":2,"checked":"` + now.Add(-10*time.Minute).UTC().Format(time.RFC3339) + `",
		"commits":[{"sha":"` + strings.Repeat("c", 40) + `","message":"Second","date":"2026-11-02T00:00:00+00:00"},{"sha":"` + strings.Repeat("b", 40) + `","message":"First","date":"2026-11-01T00:00:00Z"}]}`
	_ = os.WriteFile(filepath.Join(dir, "available.json"), []byte(avail), 0o640)
	found, checking = denmaFindUpdate(dir, "aaaaaaaa", time.Hour, now)
	if checking || asked() != "" || found.Status != "ahead" || found.Ahead != 2 || len(found.Commits) != 2 || found.Commits[0].Message != "Second" {
		t.Fatalf("a recent answer is given, without asking: %+v %v", found, checking)
	}
	if _, checking := denmaFindUpdate(dir, "aaaaaaaa", time.Minute, now); !checking || asked() != "aaaaaaaa\n" {
		t.Fatal("an older one than wanted is asked for again")
	}
	_ = os.Remove(checkFile)
	if found, checking := denmaFindUpdate(dir, "dddddddd", time.Hour, now); !checking || found.Status != "" || len(found.Commits) != 0 || asked() != "dddddddd\n" {
		t.Fatalf("an answer about another build isn't given, and this one is asked for: %+v", found)
	}

	_ = os.WriteFile(filepath.Join(dir, "status.json"), []byte(`{"state":"building","at":"`+now.UTC().Format(time.RFC3339)+`"}`), 0o640)
	if _, checking := denmaFindUpdate(dir, "dddddddd", time.Hour, now.Add(time.Hour)); !checking {
		t.Fatal("a check waiting behind an update is still checking")
	}
}

func TestDenmaRequestUpdate(t *testing.T) {
	dir := t.TempDir()
	commit := strings.Repeat("a", 40)
	if err := denmaRequestUpdate(dir, commit); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "request"))
	if err != nil || string(b) != commit+"\n" {
		t.Fatalf("got %q, %v", b, err)
	}
	if err := denmaRequestUpdate(dir, commit); err == nil {
		t.Fatal("a second request while one waits")
	}
	_ = os.Remove(filepath.Join(dir, "request"))

	at := time.Now().UTC().Format(time.RFC3339)
	_ = os.WriteFile(filepath.Join(dir, "status.json"), []byte(`{"state":"building","commit":"x","at":"`+at+`"}`), 0o640)
	if s := denmaReadUpdateState(dir); s.State != "building" {
		t.Fatalf("got %+v", s)
	}
	if err := denmaRequestUpdate(dir, commit); err == nil {
		t.Fatal("a request while one is being built")
	}
	old := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	_ = os.WriteFile(filepath.Join(dir, "status.json"), []byte(`{"state":"installing","commit":"x","at":"`+old+`"}`), 0o640)
	if s := denmaReadUpdateState(dir); s.State != "failed" || !strings.Contains(s.Message, "stopped") {
		t.Fatalf("an update under way for hours was stopped: %+v", s)
	}
	_ = os.WriteFile(filepath.Join(dir, "status.json"), []byte(`{"state":"failed"}`), 0o640)
	if err := denmaRequestUpdate(dir, commit); err != nil {
		t.Fatalf("after a failure: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("left behind: %v", entries)
	}
}
