package main

import (
	"testing"

	"github.com/knadh/koanf/v2"
)

// The archive stays off even with its setting on.
func TestDenmaNoArchive(t *testing.T) {
	ko := koanf.New(".")
	_ = ko.Set("app.enable_public_archive", true)
	denmaNoArchive(ko)
	if ko.Bool("app.enable_public_archive") {
		t.Fatal("the archive is on")
	}
}
