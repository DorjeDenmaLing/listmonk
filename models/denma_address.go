package models

// denma: a center's postal address in an e-mail's plain text
// (cmd/denma_address.go). {{ CenterAddress }} gives its lines as HTML, so a
// campaign's or transactional e-mail's plain text gives them as text.

import "regexp"

var reDenmaAddressTag = regexp.MustCompile(`(\{\{-?\s*)CenterAddress(\s*-?\}\})`)

// DenmaAddressText swaps {{ CenterAddress }} for its plain text.
func DenmaAddressText(s string) string {
	return reDenmaAddressTag.ReplaceAllString(s, "${1}CenterAddressText${2}")
}

// DenmaAltText is a campaign's plain text to compile: its links tracked, and
// the address as text.
func DenmaAltText(s string) string {
	return DenmaTrackText(DenmaAddressText(s))
}
