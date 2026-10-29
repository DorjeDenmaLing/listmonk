package main

import (
	"fmt"
	"testing"

	"github.com/knadh/koanf/v2"
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
