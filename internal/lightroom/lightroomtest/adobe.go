package lightroomtest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Adobe is a fake of the Lightroom Partner API (lr.adobe.io) as documented
// in github.com/AdobeDocs/lightroom-partner-apis: every JSON body starts
// with while(1){}, every call needs X-API-Key and a bearer token, create is
// create-only (403 "already exists"), album lists page through links.next,
// and adding to an album answers 201 / 403 with per-asset errors. Safe for
// concurrent use.
type Adobe struct {
	*httptest.Server

	APIKey    string
	CatalogID string
	AccountID string
	PageSize  int // albums per page (default 2, to exercise paging)

	mu       sync.Mutex
	token    string
	assets   map[string]*AdobeAsset
	albums   map[string]*Album
	members  map[string]map[string]bool // album → asset → in it
	refuse   map[string]bool            // albums that 403 every add
	failWith int
	calls    map[string]int
}

// AdobeAsset is what the fake stores per asset.
type AdobeAsset struct {
	FileName         string
	ImportedOnDevice string
	ImportedBy       string
	MasterSHA256     string
	MasterType       string
}

// Album is a collection or collection set.
type Album struct {
	ID, Name, Subtype, ParentID string
}

// NewAdobe starts the fake; the caller closes it.
func NewAdobe(apiKey, token string) *Adobe {
	s := &Adobe{
		APIKey: apiKey, token: token, PageSize: 2,
		CatalogID: "c0ffee00c0ffee00c0ffee00c0ffee00",
		AccountID: "acc0acc0acc0acc0acc0acc0acc0acc0",
		assets:    map[string]*AdobeAsset{}, albums: map[string]*Album{},
		members: map[string]map[string]bool{}, refuse: map[string]bool{}, calls: map[string]int{},
	}
	mux := http.NewServeMux()
	cat := "/v2/catalogs/{cid}"
	mux.HandleFunc("GET /v2/account", s.wrap("get_account", s.account))
	mux.HandleFunc("GET /v2/catalog", s.wrap("get_catalog", s.catalog))
	mux.HandleFunc("PUT "+cat+"/assets/{id}", s.wrap("create_asset", s.create))
	mux.HandleFunc("GET "+cat+"/assets/{id}", s.wrap("get_asset", s.getAsset))
	mux.HandleFunc("PUT "+cat+"/assets/{id}/master", s.wrap("upload_master", s.master))
	mux.HandleFunc("GET "+cat+"/albums", s.wrap("list_albums", s.listAlbums))
	mux.HandleFunc("PUT "+cat+"/albums/{aid}/assets", s.wrap("add_to_album", s.addToAlbum))
	mux.HandleFunc("GET "+cat+"/albums/{aid}/assets", s.wrap("list_album_assets", s.albumAssets))
	s.Server = httptest.NewServer(mux)
	return s
}

// SetCatalogID changes the user's catalogue ID (Adobe says it can).
func (s *Adobe) SetCatalogID(id string) { s.mu.Lock(); s.CatalogID = id; s.mu.Unlock() }

// SetToken changes the token the fake accepts (an old one now gets 403 4300).
func (s *Adobe) SetToken(t string) { s.mu.Lock(); s.token = t; s.mu.Unlock() }

// AddAlbum adds a collection ("collection") or set ("collection_set").
func (s *Adobe) AddAlbum(a Album) { s.mu.Lock(); s.albums[a.ID] = &a; s.mu.Unlock() }

// Refuse makes every add to album id fail with 403.
func (s *Adobe) Refuse(id string) { s.mu.Lock(); s.refuse[id] = true; s.mu.Unlock() }

// Fail makes every call answer status (0 heals).
func (s *Adobe) Fail(status int) { s.mu.Lock(); s.failWith = status; s.mu.Unlock() }

// PutMember puts an asset into an album directly.
func (s *Adobe) PutMember(album, asset string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.members[album] == nil {
		s.members[album] = map[string]bool{}
	}
	s.members[album][asset] = true
}

// InAlbum reports whether asset is in album.
func (s *Adobe) InAlbum(album, asset string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.members[album][asset]
}

// Asset returns a copy of the asset.
func (s *Adobe) Asset(id string) (AdobeAsset, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.assets[id]
	if !ok {
		return AdobeAsset{}, false
	}
	return *a, true
}

// Calls returns how often op answered status.
func (s *Adobe) Calls(op string, status int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[op+":"+strconv.Itoa(status)]
}

type reply struct {
	status int
	body   any // nil: empty body
}

func errReply(status int, code any, desc string, fields map[string][]string) reply {
	b := map[string]any{"code": code, "description": desc}
	if fields != nil {
		b["errors"] = fields
	}
	return reply{status, b}
}

func (s *Adobe) wrap(op string, h func(*http.Request) reply) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		failWith, token, catalogID := s.failWith, s.token, s.CatalogID
		s.mu.Unlock()
		var rep reply
		auth := r.Header.Get("Authorization")
		switch {
		case r.Header.Get("X-API-Key") != s.APIKey:
			rep = reply{http.StatusForbidden, map[string]any{"error_code": "403003", "message": "Api Key is invalid"}}
		case auth == "":
			rep = reply{status: http.StatusUnauthorized}
		case auth != "Bearer "+token:
			rep = errReply(http.StatusForbidden, 4300, "Access is forbidden", nil)
		case failWith != 0:
			rep = errReply(failWith, failWith, "injected fault", nil)
		case r.PathValue("cid") != "" && r.PathValue("cid") != catalogID:
			rep = errReply(http.StatusNotFound, 1000, "Resource not found", map[string][]string{"catalog": {"does not exist"}})
		default:
			rep = h(r)
		}
		s.mu.Lock()
		s.calls[op+":"+strconv.Itoa(rep.status)]++
		s.mu.Unlock()
		if rep.body == nil {
			w.WriteHeader(rep.status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rep.status)
		_, _ = io.WriteString(w, "while (1) {}\n")
		_ = json.NewEncoder(w).Encode(rep.body)
	}
}

// base is the "base" of a catalogue response; wrap checked the ID.
func (s *Adobe) base(r *http.Request) string {
	return s.URL + "/v2/catalogs/" + r.PathValue("cid") + "/"
}

func (s *Adobe) account(*http.Request) reply {
	return reply{http.StatusOK, map[string]any{
		"id": s.AccountID, "type": "account", "full_name": "Ada Lovelace", "email": "ada@example.com",
		"entitlement": map[string]any{"status": "subscriber"},
	}}
}

func (s *Adobe) catalog(*http.Request) reply {
	s.mu.Lock()
	defer s.mu.Unlock()
	return reply{http.StatusOK, map[string]any{"base": s.URL + "/v2/", "id": s.CatalogID, "type": "catalog"}}
}

func (s *Adobe) create(r *http.Request) reply {
	var req struct {
		Subtype string `json:"subtype"`
		Payload struct {
			CaptureDate  string `json:"captureDate"`
			ImportSource struct {
				FileName         string `json:"fileName"`
				ImportedOnDevice string `json:"importedOnDevice"`
				ImportedBy       string `json:"importedBy"`
				ImportTimestamp  string `json:"importTimestamp"`
			} `json:"importSource"`
		} `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return errReply(http.StatusBadRequest, 1005, "Input validation error", nil)
	}
	is := req.Payload.ImportSource
	if req.Subtype != "image" || req.Payload.CaptureDate == "" || is.FileName == "" ||
		is.ImportedOnDevice == "" || is.ImportedBy == "" || is.ImportTimestamp == "" {
		return errReply(http.StatusBadRequest, 1005, "Input validation error", nil)
	}
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.assets[id]; ok {
		return errReply(http.StatusForbidden, 1003, "The resource already exists", map[string][]string{"asset": {"already exists"}})
	}
	s.assets[id] = &AdobeAsset{FileName: is.FileName, ImportedOnDevice: is.ImportedOnDevice, ImportedBy: is.ImportedBy}
	return reply{status: http.StatusCreated}
}

func (s *Adobe) getAsset(r *http.Request) reply {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.assets[id]
	if !ok {
		return errReply(http.StatusNotFound, 1000, "Resource not found", map[string][]string{"asset": {"does not exist"}})
	}
	is := map[string]any{"fileName": a.FileName, "importedOnDevice": a.ImportedOnDevice, "importedBy": a.ImportedBy}
	if a.MasterSHA256 != "" {
		is["sha256"] = a.MasterSHA256
	}
	return reply{http.StatusOK, map[string]any{"id": id, "type": "asset", "subtype": "image",
		"payload": map[string]any{"importSource": is}}}
}

func (s *Adobe) master(r *http.Request) reply {
	ct := r.Header.Get("Content-Type")
	if ct != "image/jpeg" && ct != "application/octet-stream" {
		return errReply(http.StatusUnsupportedMediaType, 1007, "Invalid content type", nil)
	}
	h := sha256.New()
	if _, err := io.Copy(h, r.Body); err != nil {
		return errReply(http.StatusBadRequest, 1005, err.Error(), nil)
	}
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.assets[id]
	switch {
	case !ok:
		return errReply(http.StatusNotFound, 1000, "Resource not found", map[string][]string{"asset": {"does not exist"}})
	case a.MasterSHA256 != "":
		return errReply(http.StatusForbidden, 1003, "The resource already exists", map[string][]string{"master": {"already exists"}})
	}
	a.MasterSHA256, a.MasterType = hex.EncodeToString(h.Sum(nil)), ct
	return reply{status: http.StatusCreated}
}

func (s *Adobe) listAlbums(r *http.Request) reply {
	q := r.URL.Query()
	subtype, after := q.Get("subtype"), q.Get("name_after")
	s.mu.Lock()
	var list []*Album
	for _, a := range s.albums {
		if a.Subtype == subtype && a.Name > after {
			list = append(list, a)
		}
	}
	s.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	res := map[string]any{"base": s.base(r)}
	if len(list) > s.PageSize {
		list = list[:s.PageSize]
		res["links"] = map[string]any{"next": map[string]string{
			"href": "albums?subtype=" + url.QueryEscape(subtype) + "&name_after=" + url.QueryEscape(list[len(list)-1].Name),
		}}
	}
	resources := []any{}
	for _, a := range list {
		payload := map[string]any{"name": a.Name}
		if a.ParentID != "" {
			payload["parent"] = map[string]string{"id": a.ParentID}
		}
		resources = append(resources, map[string]any{"id": a.ID, "type": "album", "subtype": a.Subtype, "payload": payload})
	}
	res["resources"] = resources
	return reply{http.StatusOK, res}
}

func (s *Adobe) addToAlbum(r *http.Request) reply {
	var req struct {
		Resources []struct {
			ID      string         `json:"id"`
			Payload map[string]any `json:"payload"`
		} `json:"resources"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Resources) == 0 || len(req.Resources) > 50 {
		return errReply(http.StatusBadRequest, 1005, "Input validation error", nil)
	}
	aid := r.PathValue("aid")
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.albums[aid]; !ok || a.Subtype != "collection" && a.Subtype != "project" {
		return errReply(http.StatusNotFound, 1000, "Resource not found", map[string][]string{"album": {"does not exist"}})
	}
	var added, errs []any
	for _, res := range req.Resources {
		switch {
		case res.Payload == nil:
			errs = append(errs, map[string]any{"id": res.ID, "http_status": 400, "code": 1005, "description": "payload required"})
		case s.refuse[aid]:
			errs = append(errs, map[string]any{"id": res.ID, "http_status": 403, "code": 4300,
				"description": "Access is forbidden", "subtype": "ForbiddenError"})
		case s.members[aid][res.ID]:
			errs = append(errs, map[string]any{"id": res.ID, "http_status": 403, "code": 1003,
				"description": "The resource already exists", "errors": map[string][]string{"asset": {"already exists"}}})
		case s.assets[res.ID] == nil:
			errs = append(errs, map[string]any{"id": res.ID, "http_status": 404, "code": 1000, "description": "Resource not found"})
		default:
			if s.members[aid] == nil {
				s.members[aid] = map[string]bool{}
			}
			s.members[aid][res.ID] = true
			added = append(added, map[string]any{"id": res.ID, "href": fmt.Sprintf("albums/%s/assets/%s", aid, res.ID)})
		}
	}
	body := map[string]any{"base": s.base(r)}
	if len(errs) > 0 {
		body["errors"] = errs
	}
	if len(added) == 0 {
		return reply{http.StatusForbidden, body}
	}
	body["resources"] = added
	return reply{http.StatusCreated, body}
}

func (s *Adobe) albumAssets(r *http.Request) reply {
	aid := r.PathValue("aid")
	ids := strings.Split(r.URL.Query().Get("asset_ids"), ",")
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.albums[aid]; !ok {
		return errReply(http.StatusNotFound, 1000, "Resource not found", map[string][]string{"album": {"does not exist"}})
	}
	res := []any{}
	for _, id := range ids {
		if s.members[aid][id] {
			res = append(res, map[string]any{"id": "aa" + id[2:], "type": "album_asset",
				"asset": map[string]any{"id": id}})
		}
	}
	return reply{http.StatusOK, map[string]any{"base": s.base(r), "album": map[string]any{"id": aid}, "resources": res}}
}

// IMS is a fake of Adobe IMS's token endpoint (token/v3). It hands out
// AccessN / RefreshN tokens, each access token valid for ExpiresIn.
type IMS struct {
	*httptest.Server
	ClientID, ClientSecret string
	Code                   string // the one valid authorization code
	ExpiresIn              time.Duration

	mu        sync.Mutex
	n         int
	refreshes int
	refresh   string // the valid refresh token
	revoked   bool
}

// NewIMS starts the fake; the caller closes it.
func NewIMS(clientID, secret, code string) *IMS {
	s := &IMS{ClientID: clientID, ClientSecret: secret, Code: code, ExpiresIn: 24 * time.Hour}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ims/token/v3", s.token)
	s.Server = httptest.NewServer(mux)
	return s
}

// Revoke makes the current refresh token invalid.
func (s *IMS) Revoke() { s.mu.Lock(); s.revoked = true; s.mu.Unlock() }

// Refreshes is how many refresh grants succeeded.
func (s *IMS) Refreshes() int { s.mu.Lock(); defer s.mu.Unlock(); return s.refreshes }

// Last is the newest access token handed out.
func (s *IMS) Last() string { s.mu.Lock(); defer s.mu.Unlock(); return "access-" + strconv.Itoa(s.n) }

func (s *IMS) token(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fail := func(code string) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": code})
	}
	if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.ParseForm() != nil {
		fail("invalid_request")
		return
	}
	if r.PostForm.Get("client_id") != s.ClientID || r.PostForm.Get("client_secret") != s.ClientSecret {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		if r.PostForm.Get("code") != s.Code {
			fail("invalid_grant")
			return
		}
		s.Code = "" // one use
	case "refresh_token":
		if s.revoked || r.PostForm.Get("refresh_token") != s.refresh {
			fail("invalid_grant")
			return
		}
		s.refreshes++
	default:
		fail("unsupported_grant_type")
		return
	}
	s.n++
	s.refresh = "refresh-" + strconv.Itoa(s.n)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": "access-" + strconv.Itoa(s.n), "refresh_token": s.refresh,
		"token_type": "bearer", "expires_in": int(s.ExpiresIn.Seconds()),
	})
}
