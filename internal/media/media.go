// Package media validates local Maker listing images and MP4 metadata. MP4
// inspection checks the container and sample tables; it does not decode codecs.
package media

import (
	"crypto/sha256"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type Asset struct {
	Path, SHA256, MIME string
	Size               int64
	Width, Height      int
}

// Inspect validates and hashes the same opened regular file. Parent directory
// aliases are resolved, but a symlink at the final file component is rejected.
func Inspect(path, kind string) (Asset, error) {
	var a Asset
	var limit int64
	var w, h int
	switch kind {
	case "icon":
		limit, w, h = 2<<20, 288, 288
	case "thumbnail":
		limit, w, h = 5<<20, 1920, 960
	case "gallery":
		limit, w, h = 10<<20, 1920, 960
	default:
		return a, fmt.Errorf("unknown media kind %q", kind)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return a, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return a, err
	}
	abs = filepath.Join(parent, filepath.Base(abs))
	info, err := os.Lstat(abs)
	if err != nil {
		return a, err
	}
	if !info.Mode().IsRegular() {
		return a, fmt.Errorf("media must be a regular file, not a symlink")
	}
	f, err := os.Open(abs)
	if err != nil {
		return a, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return a, err
	}
	if !stat.Mode().IsRegular() || !os.SameFile(info, stat) {
		return a, fmt.Errorf("media file changed while opening")
	}
	ext := strings.ToLower(filepath.Ext(abs))
	video := ext == ".mp4"
	if video {
		if kind != "gallery" {
			return a, fmt.Errorf("MP4 is supported only in gallery")
		}
		limit, w, h = 50<<20, 1920, 1080
	}
	if stat.Size() <= 0 || stat.Size() > limit {
		return a, fmt.Errorf("%s media must be nonempty and at most %d bytes", kind, limit)
	}
	mime := ""
	if video {
		ww, hh, e := inspectMP4(f, stat.Size())
		if e != nil {
			return a, fmt.Errorf("invalid MP4: %w", e)
		}
		if ww != w || hh != h {
			return a, fmt.Errorf("gallery MP4 must be %dx%d, got %dx%d", w, h, ww, hh)
		}
		mime = "video/mp4"
	} else {
		if ext != ".png" && ext != ".jpg" && ext != ".jpeg" {
			return a, fmt.Errorf("images must use .png, .jpg, or .jpeg")
		}
		cfg, format, e := image.DecodeConfig(f)
		if e != nil {
			return a, fmt.Errorf("invalid image header: %w", e)
		}
		if format != "png" && format != "jpeg" {
			return a, fmt.Errorf("unsupported image format %q", format)
		}
		if (format == "png") != (ext == ".png") {
			return a, fmt.Errorf("image extension does not match %s contents", format)
		}
		if cfg.Width != w || cfg.Height != h {
			return a, fmt.Errorf("%s image must be %dx%d, got %dx%d", kind, w, h, cfg.Width, cfg.Height)
		}
		if _, e = f.Seek(0, io.SeekStart); e != nil {
			return a, e
		}
		decoded, fullFormat, e := image.Decode(f)
		if e != nil {
			return a, fmt.Errorf("invalid image data: %w", e)
		}
		if fullFormat != format || decoded.Bounds().Dx() != w || decoded.Bounds().Dy() != h {
			return a, fmt.Errorf("image dimensions changed while decoding")
		}
		mime = "image/" + format
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return a, err
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(f, limit+1))
	if err != nil {
		return a, err
	}
	after, err := f.Stat()
	if err != nil {
		return a, err
	}
	if n != stat.Size() || after.Size() != stat.Size() || !after.ModTime().Equal(stat.ModTime()) {
		return a, fmt.Errorf("media file changed during inspection")
	}
	return Asset{Path: abs, SHA256: fmt.Sprintf("%x", hash.Sum(nil)), MIME: mime, Size: n, Width: w, Height: h}, nil
}
