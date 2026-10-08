package main

import (
	"strings"
	"testing"
)

func TestDenmaAutoLoop(t *testing.T) {
	move := denmaAutoNode{Name: "move", Trigger: denmaTrigConfirms, Lists: []int64{7}, AddLists: []int64{5}}
	welcome := denmaAutoNode{Name: "welcome", Trigger: denmaTrigJoins, Lists: []int64{5}, AddTags: []string{"new"}}
	tagged := denmaAutoNode{Name: "tagged", Trigger: denmaTrigTagged, Tags: []string{"new"}}
	clicks := denmaAutoNode{Name: "clicks", Trigger: denmaTrigClicks, AddLists: []int64{7}}

	for _, c := range []struct {
		name  string
		nodes []denmaAutoNode
		want  string // the loop, joined with " ", or ""
	}{
		{"none", nil, ""},
		{"a chain", []denmaAutoNode{move, welcome, tagged, clicks}, ""},
		{"itself, by list", []denmaAutoNode{{Name: "self", Trigger: denmaTrigJoins, Lists: []int64{1}, AddLists: []int64{1}}}, "self self"},
		{"itself, by tag", []denmaAutoNode{{Name: "self", Trigger: denmaTrigTagged, Tags: []string{"x"}, AddTags: []string{"x"}}}, "self self"},
		{"two lists", []denmaAutoNode{move, {Name: "back", Trigger: denmaTrigJoins, Lists: []int64{5}, AddLists: []int64{7}}}, "move back move"},
		{"through a tag", []denmaAutoNode{clicks, move, welcome, {Name: "retag", Trigger: denmaTrigTagged, Tags: []string{"new"}, AddLists: []int64{7}}},
			"move welcome retag move"},
		{"campaigns don't loop", []denmaAutoNode{clicks, {Name: "c2", Trigger: denmaTrigOpens, AddLists: []int64{7}}}, ""},
	} {
		got := strings.Join(denmaAutoLoop(c.nodes), " ")
		if got != c.want {
			t.Errorf("%s: loop %q, want %q", c.name, got, c.want)
		}
	}
}
