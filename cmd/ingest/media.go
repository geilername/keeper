package main

import (
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rwcarlsen/goexif/exif"
	"golang.org/x/image/draw"
)

type meta struct {
	capturedAt time.Time
	model      string
}

// readMeta takes the capture time and camera model from EXIF. Bursts are
// grouped by capture time, so sub-second precision matters. Without EXIF
// the file's modification time stands in.
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

const thumbEdge = 480

// makeThumbnail writes a JPEG whose long edge is thumbEdge pixels.
func makeThumbnail(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	img, err := jpeg.Decode(f)
	if err != nil {
		return err
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w >= h {
		h, w = max(1, h*thumbEdge/w), thumbEdge
	} else {
		w, h = max(1, w*thumbEdge/h), thumbEdge
	}
	thumb := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.ApproxBiLinear.Scale(thumb, thumb.Bounds(), img, b, draw.Src, nil)

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := jpeg.Encode(tmp, thumb, &jpeg.Options{Quality: 82}); err != nil {
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
