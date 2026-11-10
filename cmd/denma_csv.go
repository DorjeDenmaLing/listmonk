package main

// denma: CSV exports whose cells a spreadsheet reads as text, never as formulas.

import (
	"encoding/csv"
)

// denmaCSVWriter writes CSV rows, each cell that a spreadsheet would read as a formula starting with an apostrophe.
type denmaCSVWriter struct{ *csv.Writer }

func denmaCSV(w *csv.Writer) denmaCSVWriter { return denmaCSVWriter{w} }

// Write writes the row with its cells made safe.
func (w denmaCSVWriter) Write(row []string) error {
	out := make([]string, len(row))
	for i, v := range row {
		out[i] = denmaCSVCell(v)
	}
	return w.Writer.Write(out)
}

// denmaCSVCell starts a cell that begins like a formula with an apostrophe.
func denmaCSVCell(v string) string {
	if v != "" && (v[0] == '=' || v[0] == '+' || v[0] == '-' || v[0] == '@' || v[0] == '\t' || v[0] == '\r') {
		return "'" + v
	}
	return v
}
