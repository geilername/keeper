package main

import (
	"os"
	"path/filepath"
	"testing"
)

// KEEPER_RAW_DIR=$PWD/test-pics go test -v -run RealRaw ./cmd/ingest/
func TestRealRawMetadata(t *testing.T) {
	dir := os.Getenv("KEEPER_RAW_DIR")
	if dir == "" {
		t.Skip("KEEPER_RAW_DIR not set")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.[aA][rR][wW]"))
	for _, f := range files {
		fi, _ := os.Stat(f)
		m := readMeta(f)
		t.Logf("%s  captured %s  model %q  orientation %d", filepath.Base(f), m.capturedAt.Format("2006-01-02 15:04:05.000"), m.model, m.orientation)
		if m.model == "" {
			t.Errorf("%s: no camera model in EXIF", filepath.Base(f))
		}
		if m.capturedAt.Equal(fi.ModTime().UTC()) {
			t.Errorf("%s: capture time fell back to the file time; EXIF DateTimeOriginal not read", filepath.Base(f))
		}
	}
}
