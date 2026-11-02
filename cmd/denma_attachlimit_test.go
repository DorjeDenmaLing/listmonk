package main

import (
	"strings"
	"testing"
)

func TestDenmaAttachRefusal(t *testing.T) {
	for _, c := range []struct {
		count int
		size  int64
		maxMB int
		want  string
	}{
		{0, 0, 0, ""},
		{0, 0, 10, ""},
		{1, 10 << 20, 10, ""},
		{1, 12 << 20, 10, "12.0 MB"},
		{1, 12 << 20, 25, ""},
		{1, 100, 0, "doesn't send attachments"},
	} {
		got := denmaAttachRefusal(c.count, c.size, c.maxMB)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%d files, %d bytes, %d MB: got %q", c.count, c.size, c.maxMB, got)
		}
	}
}
