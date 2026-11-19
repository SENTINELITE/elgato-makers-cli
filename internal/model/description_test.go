package model

import "testing"

func TestSameListingDescription(t *testing.T) {
	for _, tc := range []struct {
		name, a, b string
		want       bool
	}{
		{"exact", "Synthetic description", "Synthetic description", true},
		{"trailing LF", "Synthetic description\n", "Synthetic description", true},
		{"trailing CRLF", "Synthetic description\r\n", "Synthetic description", true},
		{"trailing line endings both strings", "Synthetic description\n\r\n", "Synthetic description\r\n\n", true},
		{"multiline preserved", "First\nSecond\n", "First\nSecond", true},
		{"HTML preserved", "<p>Synthetic</p>\n", "<p>Synthetic</p>", true},
		{"internal spacing", "Synthetic  description\n", "Synthetic description", false},
		{"leading space", " Synthetic description\n", "Synthetic description", false},
		{"leading newline", "\nSynthetic description\n", "Synthetic description", false},
		{"trailing space", "Synthetic description \n", "Synthetic description", false},
		{"trailing tab", "Synthetic description\t\n", "Synthetic description", false},
		{"internal newline", "First\nSecond\n", "First Second", false},
		{"internal CRLF", "First\r\nSecond\n", "First\nSecond", false},
		{"content changed", "Synthetic description\n", "Other description", false},
		{"HTML changed", "<p>Synthetic</p>\n", "<div>Synthetic</div>", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SameListingDescription(tc.a, tc.b); got != tc.want {
				t.Fatalf("comparison=%v want %v", got, tc.want)
			}
			if got := SameListingDescription(tc.b, tc.a); got != tc.want {
				t.Fatalf("reverse comparison=%v want %v", got, tc.want)
			}
		})
	}
}
