package artifact

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

var versionPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+){0,3}$`)
var uuidPattern = regexp.MustCompile(`^[a-z0-9]+([.-][a-z0-9]+)+$`)

// Inspect validates the exact package manifest and hashes the original ZIP bytes.
func Inspect(file string) (model.Package, error) {
	abs, err := filepath.Abs(file)
	if err != nil {
		return model.Package{}, err
	}
	result := model.Package{Path: abs}
	f, err := os.Open(file)
	if err != nil {
		return result, errors.New("cannot open package")
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return result, errors.New("package must be a regular file")
	}
	result.Size = st.Size()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return result, errors.New("cannot hash package")
	}
	result.SHA256 = hex.EncodeToString(h.Sum(nil))
	z, err := zip.NewReader(f, result.Size)
	if err != nil {
		return result, errors.New("package must be a ZIP archive")
	}
	var manifest *zip.File
	var root string
	seen := map[string]bool{}
	for _, entry := range z.File {
		n := entry.Name
		if entry.Mode()&os.ModeSymlink != 0 {
			return result, errors.New("package must not contain symbolic links")
		}
		if strings.Contains(n, "\\") || path.IsAbs(n) || path.Clean(n) != strings.TrimSuffix(n, "/") || strings.HasPrefix(n, "../") {
			return result, errors.New("unsafe archive path")
		}
		if seen[n] {
			return result, errors.New("duplicate archive path")
		}
		seen[n] = true
		if path.Base(n) == "manifest.json" {
			parts := strings.Split(n, "/")
			if len(parts) != 2 || !strings.HasSuffix(parts[0], ".sdPlugin") {
				return result, errors.New("manifest must be directly inside one .sdPlugin directory")
			}
			if manifest != nil {
				return result, errors.New("package must have exactly one manifest")
			}
			manifest = entry
			root = parts[0]
		}
	}
	if manifest == nil {
		return result, errors.New("package manifest missing")
	}
	for _, entry := range z.File {
		if entry.Name != root+"/" && !strings.HasPrefix(entry.Name, root+"/") {
			return result, errors.New("all package files must share the .sdPlugin root")
		}
	}
	if manifest.UncompressedSize64 > 1024*1024 {
		return result, errors.New("manifest too large")
	}
	r, err := manifest.Open()
	if err != nil {
		return result, errors.New("cannot read manifest")
	}
	defer r.Close()
	b, err := io.ReadAll(io.LimitReader(r, 1024*1024+1))
	if err != nil || len(b) > 1024*1024 {
		return result, errors.New("cannot read manifest")
	}
	if err = json.Unmarshal(b, &result.Manifest); err != nil {
		return result, errors.New("invalid manifest JSON")
	}
	m := result.Manifest
	if !uuidPattern.MatchString(m.UUID) || m.UUID != strings.TrimSuffix(root, ".sdPlugin") {
		return result, errors.New("manifest UUID must be a lowercase reverse-domain identifier matching the package directory")
	}
	if strings.TrimSpace(m.Name) == "" {
		return result, errors.New("manifest Name missing")
	}
	if !versionPattern.MatchString(m.Version) {
		return result, errors.New("manifest Version must contain 1-4 numeric components")
	}
	if !versionPattern.MatchString(m.Software.MinimumVersion) {
		return result, errors.New("Software.MinimumVersion must contain 1-4 numeric components")
	}
	if len(m.OS) == 0 {
		return result, errors.New("manifest OS requirements missing")
	}
	platforms := map[string]bool{}
	for _, v := range m.OS {
		if v.Platform != "mac" && v.Platform != "windows" {
			return result, fmt.Errorf("unsupported manifest OS platform %q", v.Platform)
		}
		if platforms[v.Platform] {
			return result, errors.New("duplicate OS platform")
		}
		platforms[v.Platform] = true
		if !versionPattern.MatchString(v.MinimumVersion) {
			return result, errors.New("OS.MinimumVersion must contain 1-4 numeric components")
		}
	}
	return result, nil
}
