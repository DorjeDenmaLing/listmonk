package main

import (
	"encoding/json"
	"testing"

	"github.com/knadh/koanf/v2"
)

// Every permission a form can offer has a name and an explanation.
func TestDenmaPermInfos(t *testing.T) {
	raw, err := fs.Read("/permissions.json")
	if err != nil {
		t.Fatal(err)
	}
	ko := koanf.New(".")
	_ = ko.Set("denma.multi_center", true)

	var groups []permGroup
	if err := json.Unmarshal(denmaPermissions(raw, ko), &groups); err != nil {
		t.Fatal(err)
	}
	keys := []string{denmaMCPListGet, denmaMCPListManage}
	for _, g := range denmaPermShown(groups) {
		if _, ok := denmaPermGroupLabels[g.Group]; !ok {
			t.Errorf("group %s has no name", g.Group)
		}
		keys = append(keys, g.Permissions...)
	}
	labels := map[string]string{}
	for _, k := range keys {
		info, ok := denmaPermInfos[k]
		if !ok || info.Label == "" || info.What == "" {
			t.Errorf("%s has no name or explanation", k)
		}
		if other, dup := labels[info.Label]; dup {
			t.Errorf("%s and %s are both named %q", k, other, info.Label)
		}
		labels[info.Label] = k
	}
	if len(denmaPermInfos) != len(keys) {
		t.Errorf("%d explanations for %d permissions", len(denmaPermInfos), len(keys))
	}
}

// A hidden permission isn't offered.
func TestDenmaPermShown(t *testing.T) {
	out := denmaPermShown([]permGroup{{Group: "bounces", Permissions: []string{"bounces:get", "webhooks:post_bounce"}}})
	if len(out[0].Permissions) != 1 || out[0].Permissions[0] != "bounces:get" {
		t.Errorf("got %v", out[0].Permissions)
	}
}
