package main

import (
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
