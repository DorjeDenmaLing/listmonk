package media

// Name is the file's label, or its stored name if it has none.
func (m Media) Name() string {
	if m.Label != "" {
		return m.Label
	}
	return m.Filename
}
