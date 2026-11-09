// Package spool is the layout of the shared /spool volume (ADR-3). Photos are
// content-addressed, so any service can find the bytes from the sha256 alone.
//
//	/spool/incoming/...          SFTPGo upload target (camera files, as uploaded)
//	/spool/photos/<sha256>.jpg   ingested original, written once by ingest
//	/spool/thumbs/<sha256>.jpg   thumbnail for the console, written by ingest
package spool

import (
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

func Incoming() string            { return filepath.Join(Root(), "incoming") }
func PhotoPath(sha string) string { return filepath.Join(Root(), "photos", sha+".jpg") }
func ThumbPath(sha string) string { return filepath.Join(Root(), "thumbs", sha+".jpg") }
