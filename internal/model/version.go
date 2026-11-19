package model

import (
	"strconv"
	"strings"
)

// SameStreamDeckVersion compares canonical numeric versions with one to four
// components. Missing trailing components are zero, so 1 and 1.0.0.0 agree.
// Invalid values, including overflowing components, never compare equal.
func SameStreamDeckVersion(a, b string) bool {
	left, leftOK := streamDeckVersionComponents(a)
	right, rightOK := streamDeckVersionComponents(b)
	return leftOK && rightOK && left == right
}

func streamDeckVersionComponents(version string) ([4]uint64, bool) {
	var values [4]uint64
	parts := strings.Split(version, ".")
	if len(parts) < 1 || len(parts) > len(values) {
		return values, false
	}
	for i, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return values, false
		}
		for j := 0; j < len(part); j++ {
			if part[j] < '0' || part[j] > '9' {
				return values, false
			}
		}
		value, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return values, false
		}
		values[i] = value
	}
	return values, true
}
