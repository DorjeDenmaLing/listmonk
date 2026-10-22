package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/knadh/listmonk/internal/subimporter"
)

// The import's preview reads a CSV as the importer does: its columns (a BOM
// and unknown ones aside), the lines it skips and where it stops, repeated
// addresses, the tags each address gets, and the center's attributes left out.
func TestDenmaImportRead(t *testing.T) {
	validate := func(s subimporter.SubReq) (subimporter.SubReq, error) {
		if !strings.Contains(s.Email, "@") {
			return s, errors.New("invalid e-mail")
		}
		s.Email = strings.ToLower(strings.TrimSpace(s.Email))
		if s.Name == "" {
			s.Name = "From Address"
		}
		return s, nil
	}
	csv := "\ufeffemail,Email Address,name,attributes\n" +
		`A@x.org,a,Ann,"{""tags"": [""Volunteer"", ""donor""], ""consent_at"": ""2020"", ""city"": ""Halifax""}"` + "\n" +
		"bad,b,Bad,\n" +
		"c@x.org,c\n" +
		`a@x.org,a,,"{""tags"": ""member|volunteer""}"` + "\n" +
		"d@x.org,d,Dee,{oops}\n" +
		`e@x.org,e,"Ee"x,` + "\n" +
		"f@x.org,f,Eff,\n"

	p, addrs, entries := denmaReadImport([]byte(csv), ',', validate)
	if p.Error != "" {
		t.Fatalf("error %q", p.Error)
	}
	if got := strings.Join(p.Columns, ",") + "|" + strings.Join(p.Ignored, ","); got != "email,name,attributes|Email Address" {
		t.Errorf("columns %q", got)
	}
	if p.Rows != 5 || p.Valid != 3 || p.Duplicates != 1 || p.NoName != 1 || p.BadAttribs != 1 || p.StopsAt != 7 {
		t.Errorf("rows %d valid %d duplicates %d no name %d bad attribs %d stops at %d, want 5 3 1 1 1 7",
			p.Rows, p.Valid, p.Duplicates, p.NoName, p.BadAttribs, p.StopsAt)
	}
	var lines []int
	for _, pr := range p.Problems {
		lines = append(lines, pr.Line)
	}
	if p.ProblemCount != 4 || len(lines) != 4 || lines[0] != 3 || lines[1] != 4 || lines[2] != 6 || lines[3] != 7 {
		t.Errorf("problems %d at lines %v, want 4 at 3, 4, 6, 7", p.ProblemCount, lines)
	}
	if strings.Join(addrs, ",") != "a@x.org,d@x.org" {
		t.Errorf("addresses %v", addrs)
	}
	if got := strings.Join(entries["a@x.org"].tags, ","); got != "donor,member,volunteer" {
		t.Errorf("a@x.org's tags %q, want both rows'", got)
	}
	var tags []string
	for _, tg := range p.Tags {
		tags = append(tags, tg.Tag)
	}
	if strings.Join(tags, ",") != "donor,member,volunteer" {
		t.Errorf("tags %v", tags)
	}

	s := p.Sample[0]
	if s.Line != 2 || s.Email != "a@x.org" || strings.Join(s.Tags, ",") != "donor,volunteer" ||
		strings.Join(s.Dropped, ",") != "consent_at" || len(s.Attribs) != 1 || s.Attribs["city"] != "Halifax" {
		t.Errorf("first row %+v", s)
	}
	for _, a := range p.Attribs {
		if a.Dropped != (a.Key == "consent_at") {
			t.Errorf("attribute %s dropped %v", a.Key, a.Dropped)
		}
	}

	if p, _, _ := denmaReadImport([]byte("Email Address,First Name\na@x.org,Ann\n"), ',', validate); !strings.HasPrefix(p.Error, "There's no email column") {
		t.Errorf("no email column: error %q", p.Error)
	}
}
