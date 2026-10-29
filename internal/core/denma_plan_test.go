package core

import "testing"

func TestDenmaPlanInSchema(t *testing.T) {
	ok := `[{"Plan":{"Node Type":"Nested Loop","Plans":[
		{"Node Type":"Seq Scan","Relation Name":"subscribers","Schema":"center_a"},
		{"Node Type":"Index Scan","Relation Name":"lists","Schema":"center_a"}]}}]`
	other := `[{"Plan":{"Node Type":"Nested Loop","Plans":[
		{"Node Type":"Seq Scan","Relation Name":"subscribers","Schema":"center_a"},
		{"Node Type":"Seq Scan","Relation Name":"subscribers","Schema":"center_b"}]}}]`
	noSchema := `[{"Plan":{"Node Type":"Seq Scan","Relation Name":"subscribers"}}]`
	none := `[{"Plan":{"Node Type":"Result"}}]`

	for name, c := range map[string]struct {
		plan string
		fail bool
	}{
		"current schema":   {ok, false},
		"another's schema": {other, true},
		"schema not given": {noSchema, true},
		"no relations":     {none, false},
	} {
		err := denmaPlanInSchema(c.plan, "center_a")
		if (err != nil) != c.fail {
			t.Errorf("%s: err = %v, want failure %v", name, err, c.fail)
		}
	}
	if denmaPlanInSchema("not json", "center_a") == nil {
		t.Error("bad JSON passed")
	}
}
