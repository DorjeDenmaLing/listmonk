package main

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/knadh/koanf/v2"
	"github.com/knadh/listmonk/models"
)

// SES notifications are taken from denma.ses_topics in config.toml (and,
// with the hub running, the topics SES has for its domains), and no other.
func TestDenmaSESTopicAllowed(t *testing.T) {
	if denmaHub != nil {
		t.Skip("the hub is running")
	}
	const ours = "arn:aws:sns:ca-central-1:111111111111:ses-feedback"
	ko := koanf.New(".")
	_ = ko.Set("denma.ses_topics", []string{" " + ours + " "})
	denmaSESTopics.set, denmaSESTopics.at = nil, denmaSESTopics.at.AddDate(-1, 0, 0)

	if !denmaSESTopicAllowed(ko, ours) {
		t.Error("the configured topic was refused")
	}
	for _, arn := range []string{"", "arn:aws:sns:ca-central-1:999999999999:ses-feedback"} {
		if denmaSESTopicAllowed(ko, arn) {
			t.Errorf("%q was taken", arn)
		}
	}
}

// SES's bounces of mail it didn't send (a suppression list, its validation)
// stay in their center; real hard bounces, and other providers', don't.
func TestDenmaSuppressedBounce(t *testing.T) {
	meta := func(sub string) json.RawMessage {
		return json.RawMessage(`{"notificationType":"Bounce","bounce":{"bounceType":"Permanent","bounceSubType":"` + sub + `"}}`)
	}
	for _, c := range []struct {
		b    models.Bounce
		want bool
	}{
		{models.Bounce{Source: "ses", Type: "hard", Meta: meta("OnAccountSuppressionList")}, true},
		{models.Bounce{Source: "ses", Type: "hard", Meta: meta("EmailValidationSuppressed")}, true},
		{models.Bounce{Source: "ses", Type: "hard", Meta: meta("Suppressed")}, true},
		{models.Bounce{Source: "ses", Type: "hard", Meta: meta("General")}, false},
		{models.Bounce{Source: "ses", Type: "hard", Meta: meta("NoEmail")}, false},
		{models.Bounce{Source: "ses", Type: "hard", Meta: json.RawMessage(`"not an object"`)}, false},
		{models.Bounce{Source: "postmark", Type: "hard", Meta: meta("OnAccountSuppressionList")}, false},
	} {
		if got := denmaSuppressedBounce(c.b); got != c.want {
			t.Errorf("%s %s: %v, want %v", c.b.Source, c.b.Meta, got, c.want)
		}
	}
}

// The cache of which center a UUID is in remembers, forgets, and stays
// within its limit.
func TestDenmaCenterCache(t *testing.T) {
	var c denmaCenterCache
	if _, ok := c.get("a"); ok {
		t.Fatal("an empty cache had an entry")
	}
	c.put("a", "halifax")
	if s, ok := c.get("a"); !ok || s != "halifax" {
		t.Errorf("got %q %v, want halifax", s, ok)
	}
	c.drop("a")
	if _, ok := c.get("a"); ok {
		t.Error("a dropped entry was still there")
	}
	for i := 0; i < 3*denmaCenterCacheMax; i++ {
		c.put(fmt.Sprint(i), "x")
		if len(c.m) > denmaCenterCacheMax {
			t.Fatalf("the cache has %d entries, over its limit", len(c.m))
		}
	}
	if s, ok := c.get(fmt.Sprint(3*denmaCenterCacheMax - 1)); !ok || s != "x" {
		t.Error("the newest entry was lost")
	}
}
