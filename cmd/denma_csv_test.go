package main

import (
	"bytes"
	"encoding/csv"
	"testing"
)

func TestDenmaCSV(t *testing.T) {
	var b bytes.Buffer
	w := denmaCSV(csv.NewWriter(&b))
	_ = w.Write([]string{"=HYPERLINK(\"x\")", "+1", "-2", "@SUM(A1)", "\tx", "ok", "a=b", "", "{\"a\":1}"})
	w.Flush()
	want := "\"'=HYPERLINK(\"\"x\"\")\",'+1,'-2,'@SUM(A1),'\tx,ok,a=b,,\"{\"\"a\"\":1}\"\n"
	if b.String() != want {
		t.Errorf("got %q, want %q", b.String(), want)
	}
}
