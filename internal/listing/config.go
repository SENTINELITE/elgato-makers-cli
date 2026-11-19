// Package listing validates and prepares complete Marketplace listings offline.
package listing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/SENTINELITE/elgato-makers-cli/internal/artifact"
	"github.com/SENTINELITE/elgato-makers-cli/internal/media"
	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
)

type Pricing struct {
	Type     string `json:"type"`
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}
type Assets struct {
	Icon      string   `json:"icon"`
	Thumbnail string   `json:"thumbnail"`
	Gallery   []string `json:"gallery"`
}
type Config struct {
	SchemaVersion     int               `json:"schema_version"`
	OrganizationID    string            `json:"org_id,omitempty"`
	ProductID         string            `json:"product_id,omitempty"`
	VariantID         string            `json:"variant_id,omitempty"`
	Name              string            `json:"name"`
	PackagePath       string            `json:"package"`
	DescriptionFile   string            `json:"description_file"`
	ReleaseNotesFile  string            `json:"release_notes_file"`
	Pricing           Pricing           `json:"pricing"`
	Languages         []string          `json:"languages"`
	Categories        []string          `json:"categories"`
	Links             map[string]string `json:"links"`
	Assets            Assets            `json:"assets"`
	AutoPublish       *bool             `json:"auto_publish"`
	GalleryUploadMode string            `json:"gallery_upload_mode,omitempty"`
}
type Asset struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	MIME   string `json:"mime"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Kind   string `json:"kind"`
	Order  int    `json:"order"`
}
type Prepared struct {
	Config      Config        `json:"config"`
	Package     model.Package `json:"package"`
	Description model.Notes   `json:"description"`
	Notes       model.Notes   `json:"notes"`
	Icon        Asset         `json:"icon"`
	Thumbnail   Asset         `json:"thumbnail"`
	Gallery     []Asset       `json:"gallery"`
	Digest      string        `json:"digest"`
}

// LoadConfig rejects unknown fields and resolves artifact paths against the JSON directory.
func LoadConfig(file string) (Config, error) {
	var c Config
	f, err := os.Open(file)
	if err != nil {
		return c, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return c, err
	}
	if !st.Mode().IsRegular() || st.Size() > 1024*1024 {
		return c, errors.New("listing JSON must be a regular file of at most 1 MiB")
	}
	d := json.NewDecoder(io.LimitReader(f, 1024*1024+1))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, fmt.Errorf("listing JSON: %w", err)
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return c, errors.New("listing JSON must contain exactly one object")
	}
	base, err := filepath.Abs(filepath.Dir(file))
	if err != nil {
		return c, err
	}
	resolve := func(p string) string {
		if p == "" {
			return ""
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(base, p)
		}
		return filepath.Clean(p)
	}
	c.PackagePath = resolve(c.PackagePath)
	c.DescriptionFile = resolve(c.DescriptionFile)
	c.ReleaseNotesFile = resolve(c.ReleaseNotesFile)
	c.Assets.Icon = resolve(c.Assets.Icon)
	c.Assets.Thumbnail = resolve(c.Assets.Thumbnail)
	for i, p := range c.Assets.Gallery {
		c.Assets.Gallery[i] = resolve(p)
	}
	if c.GalleryUploadMode == "" {
		c.GalleryUploadMode = "sequential"
	}
	return c, nil
}

var amountPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})(\.[0-9]{1,2})?$`)

func validate(c Config) error {
	if c.SchemaVersion != 1 {
		return errors.New("schema_version must be 1")
	}
	if !utf8.ValidString(c.Name) || strings.TrimSpace(c.Name) == "" || len(utf16.Encode([]rune(c.Name))) > 50 {
		return errors.New("name must contain 1-50 UTF-16 units")
	}
	if c.AutoPublish == nil {
		return errors.New("auto_publish must be explicitly true or false")
	}
	if c.VariantID != "" && c.ProductID == "" {
		return errors.New("variant_id requires product_id")
	}
	if c.GalleryUploadMode != "sequential" {
		return errors.New("gallery_upload_mode must be sequential")
	}
	if c.Pricing.Currency != "USD" {
		return errors.New("pricing currency must be USD")
	}
	if !amountPattern.MatchString(c.Pricing.Amount) {
		return errors.New("pricing amount must be a nonnegative decimal string with at most two decimal places")
	}
	zero := strings.Trim(c.Pricing.Amount, "0.") == ""
	if c.Pricing.Type != "free" && c.Pricing.Type != "paid" {
		return errors.New("pricing type must be free or paid")
	}
	if c.Pricing.Type == "free" && !zero {
		return errors.New("free pricing amount must be zero")
	}
	if c.Pricing.Type == "paid" && zero {
		return errors.New("paid pricing amount must be positive")
	}
	for _, list := range []struct {
		name   string
		values []string
		max    int
	}{{"languages", c.Languages, 100}, {"categories", c.Categories, 20}} {
		if len(list.values) == 0 || len(list.values) > list.max {
			return fmt.Errorf("%s must contain 1-%d entries", list.name, list.max)
		}
		seen := map[string]bool{}
		for _, v := range list.values {
			if strings.TrimSpace(v) == "" || len(v) > 128 || seen[v] {
				return fmt.Errorf("%s contains an empty, oversized or duplicate entry", list.name)
			}
			seen[v] = true
		}
	}
	if len(c.Links) > 20 {
		return errors.New("at most 20 links allowed")
	}
	for label, raw := range c.Links {
		u, err := url.Parse(raw)
		if strings.TrimSpace(label) == "" || len(label) > 100 || len(raw) > 2048 || err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
			return errors.New("links require nonempty labels and HTTPS URLs without credentials")
		}
	}
	if len(c.Assets.Gallery) < 3 || len(c.Assets.Gallery) > 10 {
		return errors.New("gallery requires 3-10 ordered PNG/JPEG images or MP4 videos")
	}
	return nil
}

// regularPath rejects symlink artifacts and canonicalizes directory aliases.
func regularPath(p string) (string, error) {
	if p == "" {
		return "", errors.New("required file path missing")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	st, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("symbolic link artifacts are not supported")
	}
	if !st.Mode().IsRegular() {
		return "", errors.New("artifact must be a regular file")
	}
	return filepath.EvalSymlinks(abs)
}
func readAsset(path, kind string, order int) (Asset, error) {
	inspected, err := media.Inspect(path, kind)
	if err != nil {
		return Asset{}, err
	}
	return Asset{Path: inspected.Path, SHA256: inspected.SHA256, Size: inspected.Size, MIME: inspected.MIME, Width: inspected.Width, Height: inspected.Height, Kind: kind, Order: order}, nil
}
func PrepareConfig(config Config) (Prepared, error) {
	// Round-trip to detach slices, maps and pointers from the caller.
	b, err := json.Marshal(config)
	if err != nil {
		return Prepared{}, err
	}
	var c Config
	if err = json.Unmarshal(b, &c); err != nil {
		return Prepared{}, err
	}
	if c.GalleryUploadMode == "" {
		c.GalleryUploadMode = "sequential"
	}
	p := Prepared{Config: c}
	if err = validate(c); err != nil {
		return p, err
	}
	for _, path := range []string{c.PackagePath, c.DescriptionFile, c.ReleaseNotesFile} {
		if _, err = regularPath(path); err != nil {
			return p, err
		}
	}
	p.Package, err = artifact.Inspect(c.PackagePath)
	if err != nil {
		return p, err
	}
	p.Description, err = artifact.ReadNotes(c.DescriptionFile)
	if err != nil {
		return p, fmt.Errorf("description: %w", err)
	}
	if p.Description.PlainTextLength < 250 || p.Description.PlainTextLength > 1500 {
		return p, errors.New("description must contain 250-1500 UTF-16 units")
	}
	p.Notes, err = artifact.ReadNotes(c.ReleaseNotesFile)
	if err != nil {
		return p, err
	}
	if p.Notes.PlainTextLength == 0 || p.Notes.PlainTextLength > 1500 {
		return p, errors.New("release notes must contain 1-1500 UTF-16 units")
	}
	p.Icon, err = readAsset(c.Assets.Icon, "icon", 0)
	if err != nil {
		return p, fmt.Errorf("icon: %w", err)
	}
	p.Thumbnail, err = readAsset(c.Assets.Thumbnail, "thumbnail", 0)
	if err != nil {
		return p, fmt.Errorf("thumbnail: %w", err)
	}
	paths, hashes := map[string]bool{}, map[string]bool{}
	for i, path := range c.Assets.Gallery {
		a, e := readAsset(path, "gallery", i+1)
		if e != nil {
			return p, fmt.Errorf("gallery %d: %w", i+1, e)
		}
		if paths[a.Path] || hashes[a.SHA256] {
			return p, errors.New("gallery contains duplicate paths or identical content")
		}
		paths[a.Path] = true
		hashes[a.SHA256] = true
		p.Gallery = append(p.Gallery, a)
	}
	p.Digest, err = preparedDigest(p)
	return p, err
}
func preparedDigest(p Prepared) (string, error) {
	p.Digest = ""
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Verify detects edited checkpoint data and artifacts before a resumed mutation.
func Verify(p Prepared) error {
	digest, err := preparedDigest(p)
	if err != nil {
		return err
	}
	if p.Digest == "" || digest != p.Digest {
		return errors.New("prepared listing digest mismatch")
	}
	fresh, err := PrepareConfig(p.Config)
	if err != nil {
		return err
	}
	if fresh.Digest != p.Digest {
		return errors.New("listing files changed since preparation")
	}
	return nil
}
