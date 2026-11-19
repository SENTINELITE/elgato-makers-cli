package cli

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/SENTINELITE/elgato-makers-cli/internal/auth"
	"github.com/SENTINELITE/elgato-makers-cli/internal/filesec"
	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
)

// All users of the same credential store serialize rotations, including import
// and forget. A crashed process leaves a lock for deliberate recovery.
func (a *app) lockSession() error {
	if a.unlockSession != nil {
		return nil
	}
	var path string
	if a.credentialsFile != "" {
		path = a.credentialsFile + ".lock"
	} else {
		base, err := os.UserCacheDir()
		if err != nil {
			return err
		}
		path = filepath.Join(base, "Elgato Makers CLI", "session.lock")
	}
	if err := filesec.CreatePrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	f, err := filesec.OpenPrivateFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return fmt.Errorf("Maker credentials are locked; verify the other process has stopped before removing %s: %w", path, err)
	}
	_, writeErr := fmt.Fprintf(f, "pid=%d\n", os.Getpid())
	closeErr := f.Close()
	if err = errors.Join(writeErr, closeErr); err != nil {
		os.Remove(path)
		return err
	}
	a.unlockSession = func() { _ = os.Remove(path) }
	return nil
}

func persistCookies(store auth.Store, api *maker.Client) error {
	var cookies []*http.Cookie
	for _, c := range api.CookieSnapshot() {
		if c.Name == "__Host-kc_at" || c.Name == "__Secure-kc_rt" || c.Name == "kc_user" {
			cookies = append(cookies, c)
		}
	}
	if len(cookies) == 0 {
		return store.Delete()
	}
	creds, err := auth.FromCookies(cookies)
	if err != nil {
		return err
	}
	return store.Save(creds)
}
