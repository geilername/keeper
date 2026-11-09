// Package spool is the layout of the shared /spool volume (ADR-3). Photos are
// content-addressed, so any service can find the bytes from the sha256 alone.
//
//	/spool/incoming/...              SFTPGo upload target (camera files, as uploaded)
//	/spool/photos/<sha256>.<ext>     the original as shot (.jpg or .arw), written once by ingest
//	/spool/previews/<sha256>.jpg     upright JPEG (long edge ≤ 2048) that the scorer analyses
//	/spool/thumbs/<sha256>.jpg       thumbnail for the console
package spool

import (
	"fmt"
	"os"
	"path/filepath"
)

// Root is the spool mount point; SPOOL_DIR overrides it (tests, local runs).
func Root() string {
	if d := os.Getenv("SPOOL_DIR"); d != "" {
		return d
	}
	return "/spool"
}

func Incoming() string                    { return filepath.Join(Root(), "incoming") }
func OriginalPath(sha, ext string) string { return filepath.Join(Root(), "photos", sha+ext) }
func PreviewPath(sha string) string       { return filepath.Join(Root(), "previews", sha+".jpg") }
func ThumbPath(sha string) string         { return filepath.Join(Root(), "thumbs", sha+".jpg") }

// FindOriginal returns the stored original for sha, whatever its format.
// Content addressing guarantees at most one.
func FindOriginal(sha string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(Root(), "photos", sha+".*"))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("spool: no original for %s: %w", sha, os.ErrNotExist)
	}
	return matches[0], nil
}
