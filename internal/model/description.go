package model

import "strings"

// SameListingDescription compares a remote listing description with reviewed
// text while allowing the provider to remove trailing CR/LF characters only.
// Spaces, markup, leading whitespace, and internal line endings stay exact.
func SameListingDescription(a, b string) bool {
	return strings.TrimRight(a, "\r\n") == strings.TrimRight(b, "\r\n")
}
