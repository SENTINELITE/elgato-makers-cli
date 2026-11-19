package model

import "testing"

func TestSameStreamDeckVersion(t *testing.T) {
	for _, tc := range []struct {
		a, b  string
		equal bool
	}{
		{"1", "1.0.0.0", true},
		{"1.2", "1.2.0.0", true},
		{"1.2.3", "1.2.3.0", true},
		{"1.2.3.4", "1.2.3.4", true},
		{"0", "0.0.0.0", true},
		{"1.2.3", "1.2.4", false},
		{"1", "1.0.0.1", false},
		{"1.2", "1.20", false},
		{"65536", "65536.0.0.0", true},
		{"18446744073709551615", "18446744073709551615.0", true},
	} {
		if got := SameStreamDeckVersion(tc.a, tc.b); got != tc.equal {
			t.Errorf("SameStreamDeckVersion(%q,%q)=%v, want %v", tc.a, tc.b, got, tc.equal)
		}
		if got := SameStreamDeckVersion(tc.b, tc.a); got != tc.equal {
			t.Errorf("reverse SameStreamDeckVersion(%q,%q)=%v, want %v", tc.b, tc.a, got, tc.equal)
		}
	}
}

func TestSameStreamDeckVersionRejectsInvalidValues(t *testing.T) {
	for _, invalid := range []string{"", " ", " 1", "1 ", "1\n", "1\r", "1\t", "+1", "-1", "1.+0", "1.-0", "01", "00", "1.00", "1.02.0", "1.0.0.00", "1.", ".1", "1..0", "1.0.0.0.0", "1-beta", "1.0+build", "１", "١", "18446744073709551616", "1.18446744073709551616", "999999999999999999999999999999999999999999"} {
		t.Run(invalid, func(t *testing.T) {
			if SameStreamDeckVersion(invalid, invalid) {
				t.Fatal("invalid version equaled itself")
			}
			if SameStreamDeckVersion(invalid, "1") || SameStreamDeckVersion("1", invalid) {
				t.Fatal("invalid version equaled valid version")
			}
		})
	}
}
