//go:build !darwin || !cgo

package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestKeychainUnavailableWithoutFallback(t *testing.T) {
	s := NewKeychainStore("synthetic", "synthetic")
	c, _ := Parse(strings.NewReader("kc_user=synthetic"))
	if _, err := s.Load(); !errors.Is(err, ErrUnavailable) {
		t.Fatal("Load must explicitly report unavailable")
	}
	if err := s.Save(c); !errors.Is(err, ErrUnavailable) {
		t.Fatal("Save must explicitly report unavailable")
	}
	if err := s.Delete(); !errors.Is(err, ErrUnavailable) {
		t.Fatal("Delete must explicitly report unavailable")
	}
}
