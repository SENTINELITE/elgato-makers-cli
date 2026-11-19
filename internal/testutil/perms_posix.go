//go:build !windows

package testutil

import "os"

func widen(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return os.Chmod(path, 0755)
	}
	return os.Chmod(path, 0644)
}
