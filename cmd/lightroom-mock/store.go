package main

import (
	"cmp"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Asset mirrors the Lightroom asset resource closely enough that a real
// adapter can swap in later. Master is a mock-only receipt of the original
// upload (the real API exposes renditions instead).
type Asset struct {
	ID      string    `json:"id"`
	Type    string    `json:"type"`    // always "asset"
	Subtype string    `json:"subtype"` // "image" or "video"
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
	Payload Payload   `json:"payload"`
}

type Payload struct {
	CaptureDate  string            `json:"captureDate,omitempty"`
	ImportSource ImportSource      `json:"importSource"`
	Ratings      map[string]Rating `json:"ratings,omitempty"` // keyed by app, as in Lightroom
	Master       *Master           `json:"master,omitempty"`
}

type ImportSource struct {
	FileName         string `json:"fileName,omitempty"`
	ImportedBy       string `json:"importedBy,omitempty"`
	ImportedOnDevice string `json:"importedOnDevice,omitempty"`
	ImportTimestamp  string `json:"importTimestamp,omitempty"`
	SHA256           string `json:"sha256,omitempty"`
}

type Rating struct {
	Rating int       `json:"rating"`
	Date   time.Time `json:"date"`
}

type Master struct {
	SizeBytes int64     `json:"sizeBytes"`
	SHA256    string    `json:"sha256"`
	Uploaded  time.Time `json:"uploaded"`
}

// ratingApp is the key under payload.ratings that Keeper writes to.
const ratingApp = "keeper"

var (
	errNotFound     = errors.New("asset not found")
	errConflict     = errors.New("asset exists with different content")
	errSHAInUse     = errors.New("sha256 already belongs to another asset")
	errMasterExists = errors.New("master already uploaded with different bytes")
)

// store is the in-memory catalogue. Every method returns copies, so callers
// never share maps with the store.
type store struct {
	mu      sync.Mutex
	assets  map[string]*Asset
	bySHA   map[string]string // importSource.sha256 -> asset ID
	replays int               // create calls that hit an existing asset
	now     func() time.Time
}

func newStore() *store {
	return &store{assets: map[string]*Asset{}, bySHA: map[string]string{}, now: time.Now}
}

func (a *Asset) clone() Asset {
	c := *a
	c.Payload.Ratings = maps.Clone(a.Payload.Ratings)
	if a.Payload.Master != nil {
		m := *a.Payload.Master
		c.Payload.Master = &m
	}
	return c
}

// create is idempotent on the asset ID: creating an existing ID returns the
// existing asset with created=false and never adds a second one. Reusing an
// ID for different content (another sha256) is a conflict.
func (s *store) create(id, subtype string, p Payload) (a Asset, created bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.assets[id]; ok {
		if p.ImportSource.SHA256 != "" && existing.Payload.ImportSource.SHA256 != "" &&
			p.ImportSource.SHA256 != existing.Payload.ImportSource.SHA256 {
			return Asset{}, false, errConflict
		}
		s.replays++
		return existing.clone(), false, nil
	}
	// Lightroom answers 412 when the sha256 already belongs to another asset.
	if sha := p.ImportSource.SHA256; sha != "" {
		if other, ok := s.bySHA[sha]; ok {
			return s.assets[other].clone(), false, errSHAInUse
		}
	}
	now := s.now().UTC()
	na := &Asset{ID: id, Type: "asset", Subtype: subtype, Created: now, Updated: now, Payload: Payload{
		CaptureDate:  p.CaptureDate,
		ImportSource: p.ImportSource,
	}}
	s.assets[id] = na
	if sha := p.ImportSource.SHA256; sha != "" {
		s.bySHA[sha] = id
	}
	return na.clone(), true, nil
}

func (s *store) get(id string) (Asset, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.assets[id]
	if !ok {
		return Asset{}, errNotFound
	}
	return a.clone(), nil
}

// list returns all assets, oldest first.
func (s *store) list() []Asset {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Asset, 0, len(s.assets))
	for _, a := range s.assets {
		out = append(out, a.clone())
	}
	slices.SortFunc(out, func(x, y Asset) int {
		return cmp.Or(x.Created.Compare(y.Created), strings.Compare(x.ID, y.ID))
	})
	return out
}

func (s *store) setRating(id string, rating int) (Asset, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.assets[id]
	if !ok {
		return Asset{}, errNotFound
	}
	now := s.now().UTC()
	if a.Payload.Ratings == nil {
		a.Payload.Ratings = map[string]Rating{}
	}
	a.Payload.Ratings[ratingApp] = Rating{Rating: rating, Date: now}
	a.Updated = now
	return a.clone(), nil
}

// setMaster records an original upload. Re-uploading the same bytes is a
// no-op (created=false); different bytes are refused.
func (s *store) setMaster(id string, size int64, sha string) (a Asset, created bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.assets[id]
	if !ok {
		return Asset{}, false, errNotFound
	}
	if m := cur.Payload.Master; m != nil {
		if m.SHA256 != sha {
			return Asset{}, false, errMasterExists
		}
		return cur.clone(), false, nil
	}
	now := s.now().UTC()
	cur.Payload.Master = &Master{SizeBytes: size, SHA256: sha, Uploaded: now}
	cur.Updated = now
	return cur.clone(), true, nil
}

func (s *store) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.assets = map[string]*Asset{}
	s.bySHA = map[string]string{}
	s.replays = 0
}

// stats is the demo summary behind GET /admin/stats.
type stats struct {
	Assets         int            `json:"assets"`
	WithMaster     int            `json:"with_master"`
	Ratings        map[string]int `json:"ratings"` // "1".."5" and "unrated"
	CreateReplays  int            `json:"create_replays"`
	FaultsInjected int            `json:"faults_injected"`
	Faults         faultConfig    `json:"faults"`
}

func (s *store) stats() stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := stats{Assets: len(s.assets), Ratings: map[string]int{}, CreateReplays: s.replays}
	for _, a := range s.assets {
		if a.Payload.Master != nil {
			st.WithMaster++
		}
		if r, ok := a.Payload.Ratings[ratingApp]; ok {
			st.Ratings[strconv.Itoa(r.Rating)]++
		} else {
			st.Ratings["unrated"]++
		}
	}
	return st
}
