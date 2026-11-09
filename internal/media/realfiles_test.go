package media

import (
	"bytes"
	"image/jpeg"
	"os"
	"path/filepath"
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
