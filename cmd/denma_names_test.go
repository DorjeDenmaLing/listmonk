package main

import (
	"testing"

	"github.com/knadh/listmonk/models"
)

// Names are split as the database splits them (once, at the first space;
// none for a stand-in), and templates' FirstName and LastName give the
// stored ones, guessing from the name only without them.
func TestDenmaNames(t *testing.T) {
	for _, c := range []struct{ name, email, first, last string }{
		{"Jane Mary Smith", "j@x.org", "Jane", "Mary Smith"},
		{"  Pema   Chödrön ", "p@x.org", "Pema", "Chödrön"},
		{"Madonna", "m@x.org", "Madonna", ""},
		{"", "e@x.org", "", ""},
		{"jane.doe", "jane.doe@x.org", "", ""},
		{"Jane Doe", "jane.doe@x.org", "", ""}, // the importer's stand-in
		{"JANE.DOE@X.ORG", "jane.doe@x.org", "", ""},
	} {
		if f, l := denmaSplitName(c.name, c.email); f != c.first || l != c.last {
			t.Errorf("denmaSplitName(%q, %q) = %q, %q; want %q, %q", c.name, c.email, f, l, c.first, c.last)
		}
	}
	if n := denmaJoinName(" Mary  Ann ", ""); n != "Mary Ann" {
		t.Errorf("denmaJoinName = %q", n)
	}

	s := models.Subscriber{Name: "Jo Smith"}
	if s.FirstName() != "Smith" {
		t.Errorf("without stored names, FirstName guesses as listmonk does: %q", s.FirstName())
	}
	s.DenmaFirstName, s.DenmaLastName = "Jo", "Smith"
	if s.FirstName() != "Jo" || s.LastName() != "Smith" {
		t.Errorf("FirstName, LastName = %q, %q; want the stored ones", s.FirstName(), s.LastName())
	}
	s.DenmaFirstName, s.DenmaLastName = "Madonna", ""
	if s.LastName() != "" {
		t.Errorf("a single name has no last name: %q", s.LastName())
	}

	// An admin's PATCH of the name alone leaves first and last names to the
	// database; one of them is sent with the attributes.
	before := models.Subscriber{Name: "Ann Lee", DenmaNames: models.DenmaNames{DenmaFirstName: "Ann", DenmaLastName: "Lee"}}
	req := before
	req.Name = "Ann Marie Lee"
	denmaNamesIn(&req, &before)
	if req.Name != "Ann Marie Lee" || req.Attribs["denma_first_name"] != nil {
		t.Errorf("a new name alone: %+v", req)
	}
	req = before
	req.DenmaLastName = "Lee-Park"
	denmaNamesIn(&req, &before)
	if req.Name != "Ann Lee-Park" || req.Attribs["denma_last_name"] != "Lee-Park" {
		t.Errorf("a new last name: %+v", req)
	}
}
