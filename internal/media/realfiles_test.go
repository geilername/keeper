package media

import (
	"bytes"
	"image/jpeg"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// Real camera files stay out of git. Point KEEPER_RAW_DIR at a local folder
// of raws to check preview extraction against them:
//
//	KEEPER_RAW_DIR=$PWD/test-pics go test -v -run RealRaw ./internal/media/
func TestRealRawPreviews(t *testing.T) {
	dir := os.Getenv("KEEPER_RAW_DIR")
	if dir == "" {
		t.Skip("KEEPER_RAW_DIR not set")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.[aA][rR][wW]"))
	if len(files) == 0 {
		t.Fatalf("no ARW files in %s", dir)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			fh, err := os.Open(f)
			if err != nil {
				t.Fatal(err)
			}
			defer fh.Close()
			fi, _ := fh.Stat()
			b, err := EmbeddedPreview(fh, fi.Size())
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := jpeg.DecodeConfig(bytes.NewReader(b))
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("preview %dx%d, %d KB", cfg.Width, cfg.Height, len(b)/1024)
			if max(cfg.Width, cfg.Height) < 1024 {
				t.Errorf("preview %dx%d is too small to score (scorer works at 1024)", cfg.Width, cfg.Height)
			}
		})
	}
}

// Embeds a rating into every raw in KEEPER_RAW_DIR, reads it back, and
// checks that only bytes inside the XMP packet changed (nothing is written
// to the directory):
//
//	KEEPER_RAW_DIR=$PWD/test-pics go test -v -run RealRawRating ./internal/media/
func TestRealRawRating(t *testing.T) {
	dir := os.Getenv("KEEPER_RAW_DIR")
	if dir == "" {
		t.Skip("KEEPER_RAW_DIR not set")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.[aA][rR][wW]"))
	if len(files) == 0 {
		t.Fatalf("no ARW files in %s", dir)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			orig, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			src := bytes.NewReader(orig)
			p, err := findXMP(src, int64(len(orig)))
			if err != nil {
				t.Fatal(err)
			}
			camera, hadCamera, _ := XMPRating(src, int64(len(orig)))
			want := 5
			if hadCamera && camera == 5 {
				want = 4
			}
			var out bytes.Buffer
			if err := EmbedRating(src, int64(len(orig)), want, &out); err != nil {
				t.Fatal(err)
			}
			got := out.Bytes()
			off, n := int(p.offset), int(p.length)
			if len(got) != len(orig) || !bytes.Equal(orig[:off], got[:off]) || !bytes.Equal(orig[off+n:], got[off+n:]) {
				t.Fatal("bytes outside the XMP packet changed")
			}
			if r, ok, err := XMPRating(bytes.NewReader(got), int64(len(got))); err != nil || !ok || r != want {
				t.Fatalf("read back %d %v %v", r, ok, err)
			}
			vals := wellFormed(t, got[off:off+n])
			if vals[nsXMP+" Rating"] != strconv.Itoa(want) {
				t.Fatalf("parsed xmp:Rating %q, want %d", vals[nsXMP+" Rating"], want)
			}
			pkt := string(got[off : off+n])
			if hadCamera && !bytes.Contains(got[off:off+n], []byte("<keeper:CameraRating>"+strconv.Itoa(camera)+"</keeper:CameraRating>")) {
				t.Fatalf("camera rating %d not kept:\n%s", camera, pkt)
			}
			if _, err := EmbeddedPreview(bytes.NewReader(got), int64(len(got))); err != nil {
				t.Fatalf("preview after embedding: %v", err)
			}
			p2, err := findXMP(bytes.NewReader(got), int64(len(got)))
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("XMP at %d (+%d), camera rating %d (%v), padding %d → %d", off, n, camera, hadCamera,
				p.trailerStart-p.contentEnd, p2.trailerStart-p2.contentEnd)
		})
	}
}
