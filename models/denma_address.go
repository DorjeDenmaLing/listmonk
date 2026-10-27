package models

// denma: what a center's e-mails show of it in plain text (cmd/denma_address.go).
// {{ CenterAddress }}, {{ CenterWebsite }} and {{ CenterEmail }} give HTML, so
// a campaign's or transactional e-mail's plain text gives them as text, and
// {{ CenterFooter }} its links as lines of text (models/denma_footer.go).

import "regexp"

var reDenmaContactTag = regexp.MustCompile(`(\{\{-?\s*)(CenterAddress|CenterWebsite|CenterEmail)(\s*-?\}\})`)

// DenmaAddressText swaps the center's tags for their plain text.
func DenmaAddressText(s string) string {
	s = reDenmaFooterTag.ReplaceAllLiteralString(s, DenmaFooterText)
	return reDenmaContactTag.ReplaceAllString(s, "${1}${2}Text${3}")
}

// DenmaAltText is a campaign's plain text to compile: its links tracked, and
// the center's tags as text.
func DenmaAltText(s string) string {
	return DenmaTrackText(DenmaAddressText(s))
}
