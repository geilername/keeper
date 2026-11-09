package main

import (
	"image"
	"image/color"
	"os"
	"path/filepath"

	"github.com/gen2brain/webp"
	"testing"
	"time"
)

func TestIncomingPath(t *testing.T) {
	t.Setenv("SPOOL_DIR", "/spool")
	for _, tc := range []struct {
		in string
		ok bool
	}{
		{"/spool/incoming/camera/DSC0001.JPG", true},
		{"/spool/incoming/../photos/x.jpg", false},
		{"/spool/incoming", false},
		{"/etc/passwd", false},
		{"", false},
	} {
		_, err := incomingPath(tc.in)
		if (err == nil) != tc.ok {
			t.Errorf("incomingPath(%q) err=%v, want ok=%v", tc.in, err, tc.ok)
		}
	}
}

func TestSubSec(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"3": 300 * time.Millisecond, "35": 350 * time.Millisecond,
		"357": 357 * time.Millisecond, "": 0, "x": 0, "357\x00": 357 * time.Millisecond,
	} {
		if got := subSec(in); got != want {
			t.Errorf("subSec(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestThumbnailIsWebP(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "preview.jpg")
	img := image.NewRGBA(image.Rect(0, 0, 2560, 1707))
	for y := 0; y < 1707; y += 7 {
		for x := 0; x < 2560; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 120, 255})
		}
	}
	if err := writeJPEG(src, img, 90); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "thumbs", "x.webp")
	if err := makeThumbnail(src, dst); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, err := webp.DecodeConfig(f)
	if err != nil {
		t.Fatalf("thumbnail is not WebP: %v", err)
	}
	if cfg.Width != thumbEdge || cfg.Height != 1707*thumbEdge/2560 {
		t.Errorf("thumbnail %dx%d, want long edge %d", cfg.Width, cfg.Height, thumbEdge)
	}
	if fi, _ := os.Stat(dst); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode %v, want 0644", fi.Mode().Perm())
	}
}
