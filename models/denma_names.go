package models

// DenmaNames are a subscriber's first and last names (cmd/denma_names.go); either can be blank.
type DenmaNames struct {
	DenmaFirstName string `db:"first_name" json:"first_name" form:"first_name"`
	DenmaLastName  string `db:"last_name" json:"last_name" form:"last_name"`
}

// denmaHasNames is whether the subscriber has a first or last name.
func (s Subscriber) denmaHasNames() bool {
	return s.DenmaFirstName != "" || s.DenmaLastName != ""
}
