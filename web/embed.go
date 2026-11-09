// Package web holds the gateway's browser UI as static files. Everything
// under static/ is embedded into the gateway binary and served at "/", so
// the console can be replaced by swapping files, without touching Go.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:static
var files embed.FS

// FS returns the static files, rooted at static/ (index.html is at "/").
func FS() fs.FS {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		panic(err) // static/ is embedded at build time; cannot be missing
	}
	return sub
}
