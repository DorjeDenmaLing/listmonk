package main

import "github.com/knadh/koanf/v2"

// denmaNoArchive turns off the public archive, whatever the settings say.
func denmaNoArchive(ko *koanf.Koanf) {
	_ = ko.Set("app.enable_public_archive", false)
}
