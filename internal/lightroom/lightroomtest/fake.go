// Package lightroomtest is a small in-memory stand-in for cmd/lightroom-mock
// (which is package main and cannot be imported). It follows the mock's
// status codes for the three calls the publisher makes, so tests can cover
// idempotent replays, conflicts and injected failures.
package lightroomtest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"time"
)

// Asset is what the fake stores per asset ID.
type Asset struct {
	SHA256       string
	FileName     string
	MasterSHA256 string
	MasterType   string // Content-Type of the uploaded original
	Rating       int
}

// Server is a fake Lightroom API. Safe for concurrent use.
type Server struct {
	*httptest.Server

	mu       sync.Mutex
	assets   map[string]*Asset
	failWith int           // status for every API call; 0 = healthy
	delay    time.Duration // added before every API call
	calls    map[string]int
}

// NewServer starts a fake for catalogID; it is closed via t.Cleanup by the
// caller (srv.Close).
func NewServer(catalogID string) *Server {
	s := &Server{assets: map[string]*Asset{}, calls: map[string]int{}}
	mux := http.NewServeMux()
	p := "/v2/catalogs/" + catalogID + "/assets/{id}"
	mux.HandleFunc("PUT "+p, s.wrap("create_asset", s.create))
	mux.HandleFunc("PUT "+p+"/master", s.wrap("upload_master", s.master))
	mux.HandleFunc("PATCH "+p, s.wrap("set_rating", s.rating))
	s.Server = httptest.NewServer(mux)
	return s
}

// Fail makes every API call answer status (0 heals).
func (s *Server) Fail(status int) { s.mu.Lock(); s.failWith = status; s.mu.Unlock() }

// Delay makes every API call wait d first.
func (s *Server) Delay(d time.Duration) { s.mu.Lock(); s.delay = d; s.mu.Unlock() }

// Put stores an asset directly, e.g. to simulate a crash after create.
func (s *Server) Put(id string, a Asset) { s.mu.Lock(); s.assets[id] = &a; s.mu.Unlock() }

// Get returns a copy of the asset and whether it exists.
func (s *Server) Get(id string) (Asset, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.assets[id]
	if !ok {
		return Asset{}, false
	}
	return *a, true
}

// Len is the number of assets.
func (s *Server) Len() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.assets) }

// Calls returns how often op answered with status (e.g. "upload_master", 201).
func (s *Server) Calls(op string, status int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[op+":"+strconv.Itoa(status)]
}

func (s *Server) wrap(op string, h func(*http.Request) (int, any)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		failWith, delay := s.failWith, s.delay
		s.mu.Unlock()
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		status, body := failWith, any(map[string]any{"code": failWith, "description": "injected fault"})
		if failWith == 0 {
			status, body = h(r)
		}
		s.mu.Lock()
		s.calls[op+":"+strconv.Itoa(status)]++
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
}

func errBody(status int, desc string) (int, any) {
	return status, map[string]any{"code": status, "description": desc}
}

func (s *Server) create(r *http.Request) (int, any) {
	var req struct {
		Payload struct {
			ImportSource struct {
				FileName string `json:"fileName"`
				SHA256   string `json:"sha256"`
			} `json:"importSource"`
		} `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return errBody(http.StatusBadRequest, err.Error())
	}
	id, sha := r.PathValue("id"), req.Payload.ImportSource.SHA256
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.assets[id]; ok {
		if a.SHA256 != sha {
			return errBody(http.StatusConflict, "asset exists with different content")
		}
		return http.StatusOK, s.resource(id, a)
	}
	for otherID, a := range s.assets {
		if a.SHA256 == sha {
			return errBody(http.StatusPreconditionFailed, "sha256 already belongs to "+otherID)
		}
	}
	a := &Asset{SHA256: sha, FileName: req.Payload.ImportSource.FileName}
	s.assets[id] = a
	return http.StatusCreated, s.resource(id, a)
}

func (s *Server) master(r *http.Request) (int, any) {
	h := sha256.New()
	if _, err := io.Copy(h, r.Body); err != nil {
		return errBody(http.StatusBadRequest, err.Error())
	}
	sum := hex.EncodeToString(h.Sum(nil))
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.assets[id]
	switch {
	case !ok:
		return errBody(http.StatusNotFound, "asset not found")
	case a.MasterSHA256 == sum:
		return http.StatusOK, s.resource(id, a)
	case a.MasterSHA256 != "":
		return errBody(http.StatusConflict, "master already uploaded with different bytes")
	}
	a.MasterSHA256 = sum
	a.MasterType = r.Header.Get("Content-Type")
	return http.StatusCreated, s.resource(id, a)
}

func (s *Server) rating(r *http.Request) (int, any) {
	var req struct {
		Rating int `json:"rating"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Rating < 1 || req.Rating > 5 {
		return errBody(http.StatusBadRequest, "rating must be between 1 and 5")
	}
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.assets[id]
	if !ok {
		return errBody(http.StatusNotFound, "asset not found")
	}
	a.Rating = req.Rating
	return http.StatusOK, s.resource(id, a)
}

// resource renders a in the mock's JSON shape. Caller holds s.mu.
func (s *Server) resource(id string, a *Asset) map[string]any {
	payload := map[string]any{"importSource": map[string]string{"fileName": a.FileName, "sha256": a.SHA256}}
	if a.MasterSHA256 != "" {
		payload["master"] = map[string]string{"sha256": a.MasterSHA256}
	}
	return map[string]any{"id": id, "type": "asset", "subtype": "image", "payload": payload}
}
