package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// errNoSet is returned when a set name is invalid, missing or has no JPEGs.
var errNoSet = errors.New("no such set")

// Set names double as directory names under SETS_DIR, so only allow plain
// names: no slashes, no "..", no hidden directories.
var setNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

type setInfo struct {
	Name   string `json:"name"`
	Frames int    `json:"frames"`
}

// listSets returns every directory under root that holds at least one JPEG,
// sorted by name.
func listSets(root string) ([]setInfo, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var sets []setInfo
	for _, e := range entries { // os.ReadDir sorts by filename
		if !e.IsDir() || !setNameRE.MatchString(e.Name()) {
			continue
		}
		frames, err := setFrames(root, e.Name())
		if err != nil {
			continue // empty or unreadable: not a set
		}
		sets = append(sets, setInfo{Name: e.Name(), Frames: len(frames)})
	}
	return sets, nil
}

// setFrames returns the JPEGs of one set as absolute paths, in filename
// order, which is the order a camera wrote them in.
func setFrames(root, name string) ([]string, error) {
	if !setNameRE.MatchString(name) {
		return nil, fmt.Errorf("%w: invalid name %q", errNoSet, name)
	}
	dir := filepath.Join(root, name)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %q", errNoSet, name)
	}
	if err != nil {
		return nil, err
	}
	var frames []string
	for _, e := range entries {
		if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".") || !isJPEG(e.Name()) {
			continue
		}
		frames = append(frames, filepath.Join(dir, e.Name()))
	}
	if len(frames) == 0 {
		return nil, fmt.Errorf("%w: %q has no JPEGs", errNoSet, name)
	}
	slices.Sort(frames)
	return frames, nil
}

func isJPEG(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg":
		return true
	}
	return false
}

// upload is one file transfer: a local frame and the name it gets on the
// FTP server.
type upload struct {
	Local  string
	Remote string
}

// planUploads maps frames to remote names (their own basename). With dup,
// the first frame is sent once more at the end under a new name: same bytes,
// same sha256, so ingest must report it as a duplicate.
func planUploads(frames []string, dup bool) []upload {
	plan := make([]upload, 0, len(frames)+1)
	for _, f := range frames {
		plan = append(plan, upload{Local: f, Remote: filepath.Base(f)})
	}
	if dup && len(frames) > 0 {
		base := filepath.Base(frames[0])
		ext := filepath.Ext(base)
		plan = append(plan, upload{
			Local:  frames[0],
			Remote: strings.TrimSuffix(base, ext) + "-dup" + ext,
		})
	}
	return plan
}
