package media

import (
	"bytes"
	"errors"
	"image"
)

// InspectRendition reads metadata from a bounded public rendition. It does not
// apply original-upload dimensions or certify codec playback/upload readiness.
func InspectRendition(data []byte, video bool) (mime string, width, height int, err error) {
	if len(data) == 0 || len(data) > 10<<20 {
		return "", 0, 0, errors.New("rendition exceeds inspection limit")
	}
	if video {
		width, height, err = inspectMP4(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return "", 0, 0, errors.New("unsupported or malformed MP4 rendition")
		}
		return "video/mp4", width, height, nil
	}
	cfg, format, e := image.DecodeConfig(bytes.NewReader(data))
	if e != nil || format != "png" && format != "jpeg" || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 8192 || cfg.Height > 8192 || int64(cfg.Width)*int64(cfg.Height) > 16_000_000 {
		return "", 0, 0, errors.New("unsupported or malformed image rendition")
	}
	decoded, format2, e := image.Decode(bytes.NewReader(data))
	if e != nil || format2 != format || decoded.Bounds().Dx() != cfg.Width || decoded.Bounds().Dy() != cfg.Height {
		return "", 0, 0, errors.New("malformed image rendition")
	}
	return "image/" + format, cfg.Width, cfg.Height, nil
}
