package listing

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"github.com/SENTINELITE/elgato-makers-cli/internal/mediatest"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "plugin.streamDeckPlugin")
	f, e := os.Create(path)
	if e != nil {
		t.Fatal(e)
	}
	z := zip.NewWriter(f)
	w, e := z.Create("com.example.plugin.sdPlugin/manifest.json")
	if e != nil {
		t.Fatal(e)
	}
	_, e = w.Write([]byte(`{"Name":"Plugin","UUID":"com.example.plugin","Version":"1.0","Software":{"MinimumVersion":"6.9"},"OS":[{"Platform":"mac","MinimumVersion":"13"}]}`))
	if e != nil {
		t.Fatal(e)
	}
	if e = z.Close(); e != nil {
		t.Fatal(e)
	}
	f.Close()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if e := os.WriteFile(p, []byte(body), 0600); e != nil {
			t.Fatal(e)
		}
		return p
	}
	img := func(name string, width, height int, col uint8) string {
		p := filepath.Join(dir, name)
		f, e := os.Create(p)
		if e != nil {
			t.Fatal(e)
		}
		m := image.NewGray(image.Rect(0, 0, width, height))
		m.SetGray(0, 0, color.Gray{Y: col})
		if e = png.Encode(f, m); e != nil {
			t.Fatal(e)
		}
		f.Close()
		return p
	}
	no := false
	return Config{SchemaVersion: 1, Name: "Plugin", PackagePath: path, DescriptionFile: write("description.md", strings.Repeat("A", 250)), ReleaseNotesFile: write("notes.md", "Initial release."), Pricing: Pricing{Type: "free", Amount: "0", Currency: "USD"}, Languages: []string{"en"}, Categories: []string{"Gaming"}, AutoPublish: &no, Assets: Assets{Icon: img("icon.png", 288, 288, 1), Thumbnail: img("thumb.png", 1920, 960, 2), Gallery: []string{img("one.png", 1920, 960, 3), img("two.png", 1920, 960, 4), img("three.png", 1920, 960, 5)}}}
}
func TestPrepareAndVerify(t *testing.T) {
	c := fixture(t)
	p, e := PrepareConfig(c)
	if e != nil {
		t.Fatal(e)
	}
	if e = Verify(p); e != nil {
		t.Fatal(e)
	}
	if p.Gallery[0].Order != 1 || p.Gallery[2].Order != 3 || p.Config.GalleryUploadMode != "sequential" {
		t.Fatal("gallery ordering/mode lost")
	}
	c.Categories[0] = "changed"
	*c.AutoPublish = true
	if p.Config.Categories[0] != "Gaming" || *p.Config.AutoPublish {
		t.Fatal("prepared config aliases caller")
	}
	if e = os.WriteFile(p.Notes.Path, []byte("Edited release."), 0600); e != nil {
		t.Fatal(e)
	}
	if e = Verify(p); e == nil {
		t.Fatal("changed file accepted")
	}
}
func TestVerifyRejectsEditedCheckpoint(t *testing.T) {
	p, e := PrepareConfig(fixture(t))
	if e != nil {
		t.Fatal(e)
	}
	p.Config.Name = "Edited"
	if Verify(p) == nil {
		t.Fatal("edited checkpoint accepted")
	}
}
func TestConfigValidation(t *testing.T) {
	base := fixture(t)
	cases := []struct {
		name string
		edit func(*Config)
	}{
		{"schema", func(c *Config) { c.SchemaVersion = 2 }},
		{"autopublish", func(c *Config) { c.AutoPublish = nil }},
		{"name UTF16", func(c *Config) { c.Name = strings.Repeat("😀", 26) }},
		{"variant", func(c *Config) { c.VariantID = "orphan" }},
		{"parallel", func(c *Config) { c.GalleryUploadMode = "parallel" }},
		{"few gallery", func(c *Config) { c.Assets.Gallery = c.Assets.Gallery[:2] }},
		{"duplicate gallery", func(c *Config) { c.Assets.Gallery = append([]string{c.Assets.Gallery[0]}, c.Assets.Gallery[:2]...) }},
		{"links", func(c *Config) { c.Links = map[string]string{"support": "http://example.com"} }},
		{"currency", func(c *Config) { c.Pricing.Currency = "EUR" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := json.Marshal(base)
			var c Config
			json.Unmarshal(b, &c)
			tc.edit(&c)
			if _, e := PrepareConfig(c); e == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}
func TestPricing(t *testing.T) {
	for _, tt := range []struct {
		kind, amount string
		valid        bool
	}{{"free", "0", true}, {"free", "0.00", true}, {"paid", "0.01", true}, {"paid", "12.99", true}, {"paid", "0", false}, {"free", "1", false}, {"paid", "01", false}, {"paid", "1.001", false}, {"paid", "1e2", false}, {"paid", "-1", false}, {"paid", "NaN", false}, {"paid", "", false}} {
		c := Config{SchemaVersion: 1, Name: "Plugin", AutoPublish: new(bool), GalleryUploadMode: "sequential", Languages: []string{"en"}, Categories: []string{"Gaming"}, Pricing: Pricing{Type: tt.kind, Amount: tt.amount, Currency: "USD"}, Assets: Assets{Gallery: []string{"1", "2", "3"}}}
		if e := validate(c); (e == nil) != tt.valid {
			t.Errorf("%s %q: %v", tt.kind, tt.amount, e)
		}
	}
}
func TestLoadStrictRelative(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "listing.json")
	for _, raw := range []string{`{"schema_version":1,"typo":true}`, `{} {}`, `{} trailing`} {
		os.WriteFile(file, []byte(raw), 0600)
		if _, e := LoadConfig(file); e == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	os.WriteFile(file, []byte(`{"schema_version":1,"package":"plugin.zip","assets":{"icon":"icon.png","gallery":["one.png"]}}`), 0600)
	c, e := LoadConfig(file)
	if e != nil {
		t.Fatal(e)
	}
	if c.PackagePath != filepath.Join(dir, "plugin.zip") || c.Assets.Gallery[0] != filepath.Join(dir, "one.png") {
		t.Fatal("relative paths not resolved")
	}
}
func TestImageFailures(t *testing.T) {
	c := fixture(t)
	tests := []struct {
		name string
		path string
	}{{"wrong shape", c.Assets.Icon}, {"wrong extension", filepath.Join(filepath.Dir(c.Assets.Icon), "fake.jpg")}, {"symlink", filepath.Join(filepath.Dir(c.Assets.Icon), "link.png")}}
	bytes, e := os.ReadFile(c.Assets.Gallery[0])
	if e != nil {
		t.Fatal(e)
	}
	os.WriteFile(tests[1].path, bytes, 0600)
	if e = os.Symlink(c.Assets.Gallery[0], tests[2].path); e != nil {
		t.Fatal(e)
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, e := readAsset(tc.path, "gallery", 1); e == nil {
				t.Fatal("bad image accepted")
			}
		})
	}
	copied := filepath.Join(filepath.Dir(c.Assets.Icon), "copy.png")
	os.WriteFile(copied, bytes, 0600)
	c.Assets.Gallery[1] = copied
	if _, e = PrepareConfig(c); e == nil {
		t.Fatal("identical gallery bytes accepted")
	}
}

func TestLinks(t *testing.T) {
	c := fixture(t)
	c.Links = map[string]string{"Support": "https://example.com/support", "Website": "https://example.org"}
	if _, err := PrepareConfig(c); err != nil {
		t.Fatalf("valid links: %v", err)
	}
	for _, raw := range []string{"http://example.com", "https://user:secret@example.com", "https:///missing-host", "javascript:alert(1)", ""} {
		c.Links = map[string]string{"Support": raw}
		if _, err := PrepareConfig(c); err == nil {
			t.Errorf("invalid URL accepted: %q", raw)
		}
	}
	c.Links = map[string]string{"": "https://example.com"}
	if _, err := PrepareConfig(c); err == nil {
		t.Error("empty link label accepted")
	}
	c.Links = map[string]string{}
	for i := 0; i < 21; i++ {
		c.Links[strings.Repeat("x", i+1)] = "https://example.com"
	}
	if _, err := PrepareConfig(c); err == nil {
		t.Error("excessive links accepted")
	}
}

func TestLinksIntentSurvivesCheckpointRoundTrip(t *testing.T) {
	base := fixture(t)
	for _, empty := range []bool{false, true} {
		c := base
		if empty {
			c.Links = map[string]string{}
		}
		prepared, err := PrepareConfig(c)
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(prepared)
		if err != nil {
			t.Fatal(err)
		}
		var restored Prepared
		if err = json.Unmarshal(data, &restored); err != nil {
			t.Fatal(err)
		}
		if (restored.Config.Links != nil) != empty {
			t.Fatalf("explicit empty=%v: links intent lost in checkpoint", empty)
		}
		if err = Verify(restored); err != nil {
			t.Fatalf("explicit empty=%v: %v", empty, err)
		}
	}
}

func writeArtwork(t *testing.T, path string, width, height int, shade uint8, jpegFormat bool) string {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	m := image.NewRGBA(image.Rect(0, 0, width, height))
	m.Set(0, 0, color.RGBA{R: shade, A: 255})
	if jpegFormat {
		err = jpeg.Encode(f, m, nil)
	} else {
		err = png.Encode(f, m)
	}
	if err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMarketplaceGalleryCounts(t *testing.T) {
	base := fixture(t)
	paths := append([]string(nil), base.Assets.Gallery...)
	for i := 3; i < 11; i++ {
		paths = append(paths, writeArtwork(t, filepath.Join(filepath.Dir(base.Assets.Icon), fmt.Sprintf("gallery-%d.png", i)), 1920, 960, uint8(i+10), false))
	}
	for _, count := range []int{0, 1, 2, 3, 10, 11} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			c := base
			c.Assets.Gallery = paths[:count]
			_, err := PrepareConfig(c)
			valid := count >= 3 && count <= 10
			if (err == nil) != valid {
				t.Fatalf("gallery count %d: %v", count, err)
			}
		})
	}
}

func TestMarketplaceDescriptionBounds(t *testing.T) {
	c := fixture(t)
	for _, count := range []int{0, 249, 250, 1500, 1501} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			if err := os.WriteFile(c.DescriptionFile, []byte(strings.Repeat("x", count)), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := PrepareConfig(c)
			valid := count >= 250 && count <= 1500
			if (err == nil) != valid {
				t.Fatalf("description length %d: %v", count, err)
			}
		})
	}
	// Limits count UTF-16 units, including surrogate pairs.
	if err := os.WriteFile(c.DescriptionFile, []byte(strings.Repeat("😀", 125)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareConfig(c); err != nil {
		t.Fatalf("250 UTF-16 units: %v", err)
	}
}

func TestMarketplaceMissingArtwork(t *testing.T) {
	base := fixture(t)
	for _, kind := range []string{"icon", "thumbnail"} {
		t.Run(kind, func(t *testing.T) {
			c := base
			if kind == "icon" {
				c.Assets.Icon = ""
			} else {
				c.Assets.Thumbnail = ""
			}
			if _, err := PrepareConfig(c); err == nil {
				t.Fatalf("missing %s accepted", kind)
			}
		})
	}
}

func TestMarketplaceArtworkRequirements(t *testing.T) {
	dir := t.TempDir()
	for _, kind := range []string{"icon", "thumbnail", "gallery"} {
		t.Run(kind, func(t *testing.T) {
			width, height := 1920, 960
			if kind == "icon" {
				width, height = 288, 288
			}
			good := writeArtwork(t, filepath.Join(dir, kind+".png"), width, height, 1, false)
			if _, err := readAsset(good, kind, 0); err != nil {
				t.Fatalf("valid PNG: %v", err)
			}
			wrongSize := writeArtwork(t, filepath.Join(dir, kind+"-wrong-size.png"), width-1, height, 2, false)
			if _, err := readAsset(wrongSize, kind, 0); err == nil {
				t.Fatal("wrong dimensions accepted")
			}
			wrongSquare := writeArtwork(t, filepath.Join(dir, kind+"-square.png"), 144, 144, 2, false)
			if _, err := readAsset(wrongSquare, kind, 0); err == nil {
				t.Fatal("undersized square accepted")
			}
			jpegPath := writeArtwork(t, filepath.Join(dir, kind+".jpg"), width, height, 3, true)
			if a, err := readAsset(jpegPath, kind, 0); err != nil || a.MIME != "image/jpeg" {
				t.Fatalf("valid JPEG rejected: %v", err)
			}
			disguised := writeArtwork(t, filepath.Join(dir, kind+"-disguised.png"), width, height, 3, true)
			if _, err := readAsset(disguised, kind, 0); err == nil {
				t.Fatal("JPEG with PNG extension accepted")
			}
			data, err := os.ReadFile(good)
			if err != nil {
				t.Fatal(err)
			}
			// Keep a valid IHDR but remove compressed image data and IEND.
			truncated := data[:len(data)/2]
			if _, err = png.DecodeConfig(bytes.NewReader(truncated)); err != nil {
				t.Fatalf("regression fixture lacks valid header: %v", err)
			}
			corrupt := filepath.Join(dir, kind+"-corrupt.png")
			if err = os.WriteFile(corrupt, truncated, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err = readAsset(corrupt, kind, 0); err == nil {
				t.Fatal("truncated PNG with valid dimensions accepted")
			}
			// A huge claimed image is rejected before pixel allocation/decompression.
			oversized := append([]byte(nil), data...)
			binary.BigEndian.PutUint32(oversized[16:20], 50000)
			binary.BigEndian.PutUint32(oversized[20:24], 50000)
			binary.BigEndian.PutUint32(oversized[29:33], crc32.ChecksumIEEE(oversized[12:29]))
			huge := filepath.Join(dir, kind+"-huge.png")
			if err = os.WriteFile(huge, oversized, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err = readAsset(huge, kind, 0); err == nil {
				t.Fatalf("oversized header must fail dimensions first: %v", err)
			}
		})
	}
}

func TestGalleryAllowsThumbnailReuseAndJPEG(t *testing.T) {
	c := fixture(t)
	jpegPath := writeArtwork(t, filepath.Join(filepath.Dir(c.Assets.Icon), "gallery.jpeg"), 1920, 960, 20, true)
	c.Assets.Gallery = []string{c.Assets.Thumbnail, jpegPath, c.Assets.Gallery[2]}
	p, err := PrepareConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	if p.Gallery[0].SHA256 != p.Thumbnail.SHA256 || p.Gallery[0].MIME != "image/png" || p.Gallery[1].MIME != "image/jpeg" {
		t.Fatal("thumbnail reuse or JPEG MIME lost")
	}
	if err = Verify(p); err != nil {
		t.Fatal(err)
	}
}

func writeMP4(t *testing.T, path string) string {
	t.Helper()
	if err := os.WriteFile(path, mediatest.MP4(), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGalleryMixedMediaAndThumbnailReuse(t *testing.T) {
	c := fixture(t)
	dir := filepath.Dir(c.Assets.Icon)
	jpg := writeArtwork(t, filepath.Join(dir, "gallery.jpg"), 1920, 960, 21, true)
	video := writeMP4(t, filepath.Join(dir, "gallery.mp4"))
	c.Assets.Gallery = []string{c.Assets.Thumbnail, jpg, video}
	p, err := PrepareConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{"image/png", "image/jpeg", "video/mp4"}
	for i, a := range p.Gallery {
		if a.MIME != expected[i] || a.Order != i+1 {
			t.Fatalf("item%d wrong MIME/order: %#v", i+1, a)
		}
	}
	if p.Gallery[0].SHA256 != p.Thumbnail.SHA256 {
		t.Fatal("thumbnail reuse changed content")
	}
	if p.Gallery[2].Width != 1920 || p.Gallery[2].Height != 1080 {
		t.Fatal("video dimensions lost")
	}
	if err = Verify(p); err != nil {
		t.Fatal(err)
	}
	p.Gallery[2].MIME = "image/png"
	if err = Verify(p); err == nil {
		t.Fatal("edited media MIME accepted")
	}
}
