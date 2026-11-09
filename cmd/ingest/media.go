package main

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gen2brain/webp"
	"github.com/rwcarlsen/goexif/exif"
	"golang.org/x/image/draw"

	"github.com/geilername/keeper/internal/media"
)

type meta struct {
	capturedAt  time.Time
	model       string
	orientation int // EXIF 1–8, 0 when unknown
}

// readMeta takes capture time, camera model and orientation from EXIF. It
// reads JPEGs and TIFF-based raws (ARW) alike. Bursts are grouped by capture
// time, so sub-second precision matters. Without EXIF the file's
// modification time stands in.
func readMeta(path string) meta {
	m := meta{capturedAt: time.Now().UTC()}
	f, err := os.Open(path)
	if err != nil {
		return m
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil {
		m.capturedAt = fi.ModTime().UTC()
	}
	x, err := exif.Decode(f)
	if err != nil {
		return m
	}
	if t, err := x.DateTime(); err == nil {
		if tag, err := x.Get(exif.SubSecTimeOriginal); err == nil {
			if s, err := tag.StringVal(); err == nil {
				t = t.Add(subSec(s))
			}
		}
		m.capturedAt = t.UTC()
	}
	if tag, err := x.Get(exif.Model); err == nil {
		if s, err := tag.StringVal(); err == nil {
			m.model = strings.TrimSpace(strings.TrimRight(s, "\x00"))
		}
	}
	if tag, err := x.Get(exif.Orientation); err == nil {
		if o, err := tag.Int(0); err == nil {
			m.orientation = o
		}
	}
	return m
}

// subSec turns EXIF SubSecTime digits ("3", "35", "357") into a duration.
func subSec(s string) time.Duration {
	s = strings.TrimSpace(strings.TrimRight(s, "\x00"))
	if s == "" || len(s) > 9 {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	for i := len(s); i < 9; i++ {
		n *= 10
	}
	return time.Duration(n)
}

const (
	previewEdge = 2560 // face detection runs at 2560 (scorer FACE_LONG_EDGE); sharpness etc. at 1024
	thumbEdge   = 480
)

var errNoPreview = errors.New("raw file has no embedded JPEG preview")

// makePreview writes the upright JPEG the scorer and the console read. An
// upright JPEG original is linked as it is; a rotated JPEG or a raw's
// embedded preview is decoded, scaled to previewEdge and turned upright.
func makePreview(original string, t media.Type, orientation int, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	if t == media.JPEG && orientation <= 1 {
		return linkOrCopy(original, dst)
	}
	img, err := decodeForPreview(original, t)
	if err != nil {
		return err
	}
	img = media.Upright(scaleTo(img, previewEdge), orientation)
	return writeJPEG(dst, img, 90)
}

func decodeForPreview(original string, t media.Type) (image.Image, error) {
	f, err := os.Open(original)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var r io.Reader = f
	if t == media.ARW {
		fi, err := f.Stat()
		if err != nil {
			return nil, err
		}
		b, err := media.EmbeddedPreview(f, fi.Size())
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errNoPreview, err)
		}
		r = bytes.NewReader(b)
	}
	return jpeg.Decode(r)
}

// makeThumbnail scales the (already upright) preview for the console and
// writes it as WebP: about a third smaller than JPEG at the same look, and it
// is what every browser and phone downloads.
func makeThumbnail(preview, dst string) error {
	f, err := os.Open(preview)
	if err != nil {
		return err
	}
	defer f.Close()
	img, err := jpeg.Decode(f)
	if err != nil {
		return err
	}
	return writeAtomic(dst, func(w io.Writer) error {
		return webp.Encode(w, scaleTo(img, thumbEdge), webp.Options{Quality: 80, Method: 4})
	})
}

// scaleTo shrinks img so its long edge is at most edge; smaller images pass.
func scaleTo(img image.Image, edge int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if max(w, h) <= edge {
		return img
	}
	if w >= h {
		h, w = max(1, h*edge/w), edge
	} else {
		w, h = max(1, w*edge/h), edge
	}
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.ApproxBiLinear.Scale(out, out.Bounds(), img, b, draw.Src, nil)
	return out
}

// writeJPEG writes atomically (temp file + rename) with mode 0644.
func writeJPEG(dst string, img image.Image, quality int) error {
	return writeAtomic(dst, func(w io.Writer) error {
		return jpeg.Encode(w, img, &jpeg.Options{Quality: quality})
	})
}

// writeAtomic writes through encode to a temp file, then renames it into
// place with mode 0644, so readers never see a half-written image.
func writeAtomic(dst string, encode func(io.Writer) error) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := encode(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil { // CreateTemp makes 0600
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
