// Package testutil gives tests platform-neutral ways to widen and inspect
// filesystem access so storage security checks run on every OS.
package testutil

import (
	"testing"
)

// Widen grants group/other (POSIX) or Everyone (Windows) broad access to path,
// making a previously private file or directory fail privacy verification.
func Widen(t *testing.T, path string) {
	t.Helper()
	if err := widen(path); err != nil {
		t.Fatal(err)
	}
}
