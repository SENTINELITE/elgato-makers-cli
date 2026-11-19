package media

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SENTINELITE/elgato-makers-cli/internal/mediatest"
)

func write(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if e := os.WriteFile(p, b, 0600); e != nil {
		t.Fatal(e)
	}
	return p
}
func imageBytes(t *testing.T, format string, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	var e error
	if format == "png" {
		e = png.Encode(&b, m)
	} else {
		e = jpeg.Encode(&b, m, nil)
	}
	if e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}
func TestImages(t *testing.T) {
	for _, format := range []string{"png", "jpeg"} {
		for _, kind := range []string{"icon", "thumbnail", "gallery"} {
			t.Run(format+kind, func(t *testing.T) {
				w, h := 1920, 960
				if kind == "icon" {
					w, h = 288, 288
				}
				b := imageBytes(t, format, w, h)
				a, e := Inspect(write(t, "a."+format, b), kind)
				if e != nil {
					t.Fatal(e)
				}
				if a.MIME != "image/"+format || a.Width != w || a.Height != h || a.SHA256 != fmt.Sprintf("%x", sha256.Sum256(b)) || a.Size != int64(len(b)) {
					t.Fatalf("unexpected asset: %+v", a)
				}
			})
		}
	}
}
func TestRejectImages(t *testing.T) {
	good := imageBytes(t, "png", 288, 288)
	cases := []struct {
		name, kind string
		b          []byte
	}{
		{"wrong.jpg", "icon", good}, {"small.png", "thumbnail", good}, {"truncated.png", "icon", good[:len(good)/2]}, {"bad.png", "icon", []byte("bad")}, {"large.png", "icon", append(good, make([]byte, (2<<20)+1)...)}, {"unsupported.gif", "icon", good}, {"unknown.png", "unknown", good},
	}
	for _, c := range cases {
		t.Run(c.name+c.kind, func(t *testing.T) {
			if _, e := Inspect(write(t, c.name, c.b), c.kind); e == nil {
				t.Fatal("accepted invalid image")
			}
		})
	}
	p := write(t, "file.png", good)
	link := filepath.Join(t.TempDir(), "link.png")
	if e := os.Symlink(p, link); e != nil {
		t.Fatal(e)
	}
	if _, e := Inspect(link, "icon"); e == nil {
		t.Fatal("accepted symlink")
	}
	parentLink := filepath.Join(t.TempDir(), "parent")
	if e := os.Symlink(filepath.Dir(p), parentLink); e != nil {
		t.Fatal(e)
	}
	a, e := Inspect(filepath.Join(parentLink, "file.png"), "icon")
	if e != nil {
		t.Fatal(e)
	}
	resolved, e := filepath.EvalSymlinks(p)
	if e != nil || a.Path != resolved {
		t.Fatalf("parent alias not canonicalized: %s", a.Path)
	}
}
func TestMP4(t *testing.T) {
	b := mediatest.MP4()
	a, e := Inspect(write(t, "a.mp4", b), "gallery")
	if e != nil {
		t.Fatal(e)
	}
	if a.MIME != "video/mp4" || a.Width != 1920 || a.Height != 1080 {
		t.Fatalf("unexpected MP4: %+v", a)
	}
	for _, kind := range []string{"icon", "thumbnail"} {
		if _, e := Inspect(write(t, "a.mp4", b), kind); e == nil {
			t.Fatal("accepted MP4 outside gallery")
		}
	}
}
func TestRejectMP4(t *testing.T) {
	good := mediatest.MP4()
	change := func(key string, delta int, v uint32) []byte {
		b := bytes.Clone(good)
		i := bytes.Index(b, []byte(key))
		if i < 0 {
			t.Fatal(key)
		}
		binary.BigEndian.PutUint32(b[i+delta:], v)
		return b
	}
	replace := func(old, new string) []byte { return bytes.Replace(bytes.Clone(good), []byte(old), []byte(new), 1) }
	cases := map[string][]byte{
		"missing-mvhd": replace("mvhd", "free"), "missing-mdhd": replace("mdhd", "free"), "missing-stts": replace("stts", "free"),
		"zero-timescale": change("mdhd", 16, 0), "zero-duration": change("mvhd", 20, 0), "unknown-movie-version": change("mvhd", 4, 0x02000000),
		"bad-timing-count": change("stts", 12, 2), "bad-timing-duration": change("stts", 16, 999), "truncated-avcc": change("avcC", -4, 12),
		"missing-pps": change("avcC", 12, 0xffffffff), "unknown-visual-metadata": replace("avcC", "xxxx"),
		"truncated": good[:len(good)-1], "hugebox": change("ftyp", -4, 0xffffffff), "badbrand": bytes.ReplaceAll(replace("isom", "xxxx"), []byte("mp42"), []byte("xxxx")),
		"no-moov": replace("moov", "free"), "no-mdat": replace("mdat", "free"), "no-video": replace("vide", "soun"), "encrypted": replace("avc1", "encv"), "unknown-codec": replace("avc1", "xxxx"), "no-codec-config": replace("avcC", "free"),
		"dimensions": change("tkhd", 80, 1280<<16), "rotation": change("tkhd", 44, 0), "many-descriptions": change("stsd", 8, 0xffffffff), "missing-samples": change("stsz", 12, 0), "bad-offset": change("stco", 12, 0xffffffff), "bad-map": change("stsc", 16, 0xffffffff), "truncated-codec": change("avcC", -4, 0xffffffff), "fragmented": replace("moov", "moof"),
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			_, e := Inspect(write(t, "a.mp4", b), "gallery")
			if e == nil {
				t.Fatal("accepted invalid MP4")
			}
		})
	}
	// No file-sized allocations even for corrupt nested/extended box headers.
	for i := 0; i < len(good); i++ {
		_, _, _ = inspectMP4(bytes.NewReader(good[:i]), int64(i))
	}
}
func FuzzMP4(f *testing.F) {
	f.Add(mediatest.MP4())
	f.Add([]byte(strings.Repeat("x", 32)))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 1_000_000 {
			t.Skip()
		}
		_, _, _ = inspectMP4(bytes.NewReader(b), int64(len(b)))
	})
}

// Optional real encoder output supplements synthetic malformed-container cases.
// The production inspector and normal test run never invoke an encoder.
func TestEncodedMP4Fixture(t *testing.T) {
	path := os.Getenv("MAKER_TEST_MP4_FIXTURE")
	if path == "" {
		t.Skip("set MAKER_TEST_MP4_FIXTURE to a local 1920x1080 MP4")
	}
	if _, err := Inspect(path, "gallery"); err != nil {
		t.Fatal(err)
	}
}

func TestConsoleByteLimits(t *testing.T) {
	cases := []struct {
		kind, ext string
		limit     int64
		w, h      int
	}{
		{"icon", "png", 2 << 20, 288, 288}, {"thumbnail", "png", 5 << 20, 1920, 960}, {"gallery", "png", 10 << 20, 1920, 960}, {"gallery", "mp4", 50 << 20, 1920, 1080},
	}
	for _, c := range cases {
		t.Run(c.kind+c.ext, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "asset."+c.ext)
			f, e := os.Create(p)
			if e != nil {
				t.Fatal(e)
			}
			if e = f.Truncate(c.limit + 1); e != nil {
				t.Fatal(e)
			}
			if e = f.Close(); e != nil {
				t.Fatal(e)
			}
			if _, e = Inspect(p, c.kind); e == nil || !strings.Contains(e.Error(), fmt.Sprintf("at most %d bytes", c.limit)) {
				t.Fatalf("expected size rejection at limit+1, got %v", e)
			}
			if c.ext == "mp4" {
				return
			}
			// A complete PNG followed by inert padding exercises the exact byte ceiling
			// without allocating a file-sized buffer. PNG decoding stops at IEND.
			b := imageBytes(t, "png", c.w, c.h)
			f, e = os.Create(p)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = f.Write(b); e != nil {
				t.Fatal(e)
			}
			if e = f.Truncate(c.limit); e != nil {
				t.Fatal(e)
			}
			if e = f.Close(); e != nil {
				t.Fatal(e)
			}
			a, e := Inspect(p, c.kind)
			if e != nil {
				t.Fatalf("exact Console limit rejected: %v", e)
			}
			if a.Size != c.limit {
				t.Fatalf("size=%d", a.Size)
			}
		})
	}
}
