package lightroom

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultAdobeURL is the Lightroom Partner API.
const DefaultAdobeURL = "https://lr.adobe.io"

// Adobe is a Client and Albums for the Lightroom Partner API
// (github.com/AdobeDocs/lightroom-partner-apis). Every call sends the API
// key and the user's bearer token from a TokenSource. The user's catalogue
// and account IDs are looked up on first use and cached; a 404 "catalog
// does not exist" drops the cached catalogue (Adobe says it can change).
//
// Idempotency on top of an API that is create-only:
//   - CreateAsset: PUT answers 403 "already exists" for a known ID, so the
//     existing asset is read back instead.
//   - UploadMaster: a master the server already has (403 "already exists")
//     is ours, because the asset ID is derived from the bytes' sha256.
//   - AddToAlbum: an asset Adobe would not add is checked against the
//     album; one that is already in it counts as added.
//
// It is safe for concurrent use.
type Adobe struct {
	baseURL string
	apiKey  string
	tokens  TokenSource
	now     func() time.Time
	caller

	mu        sync.Mutex
	catalogID string
	accountID string
}

var (
	_ Client = (*Adobe)(nil)
	_ Albums = (*Adobe)(nil)
)

// NewAdobe returns a client for baseURL (DefaultAdobeURL in production).
// apiKey is the Adobe Developer Console client ID.
func NewAdobe(baseURL, apiKey string, tokens TokenSource, timeout time.Duration) *Adobe {
	a := &Adobe{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		tokens:  tokens,
		now:     time.Now,
	}
	a.caller = caller{client: tracedClient(), timeout: timeout, auth: a.authHeaders}
	return a
}

func (a *Adobe) authHeaders(ctx context.Context, h http.Header) error {
	tok, err := a.tokens.AccessToken(ctx)
	if err != nil {
		return err
	}
	h.Set("X-API-Key", a.apiKey)
	h.Set("Authorization", "Bearer "+tok)
	return nil
}

// Account is the part of GET /v2/account Keeper shows.
type Account struct {
	ID          string `json:"id"`
	Name        string `json:"full_name"`
	Email       string `json:"email"`
	Entitlement struct {
		Status string `json:"status"` // subscriber | trial | …
	} `json:"entitlement"`
}

// DisplayName is the name to show for the account.
func (a Account) DisplayName() string {
	if a.Name != "" {
		return a.Name
	}
	return a.Email
}

// Account reads the signed-in user's account.
func (a *Adobe) Account(ctx context.Context) (Account, error) {
	var acc Account
	_, err := a.do(ctx, "get_account", http.MethodGet, "/v2/account", a.baseURL+"/v2/account", "", nil, 0, &acc)
	if err == nil {
		a.mu.Lock()
		a.accountID = acc.ID
		a.mu.Unlock()
	}
	return acc, err
}

// Catalog returns the user's catalogue ID (GET /v2/catalog, cached).
func (a *Adobe) Catalog(ctx context.Context) (string, error) {
	a.mu.Lock()
	id := a.catalogID
	a.mu.Unlock()
	if id != "" {
		return id, nil
	}
	var res struct {
		ID string `json:"id"`
	}
	if _, err := a.do(ctx, "get_catalog", http.MethodGet, "/v2/catalog", a.baseURL+"/v2/catalog", "", nil, 0, &res); err != nil {
		return "", err
	}
	if res.ID == "" {
		return "", &StatusError{Op: "get_catalog", StatusCode: http.StatusBadGateway, Description: "no catalog id"}
	}
	a.mu.Lock()
	a.catalogID = res.ID
	a.mu.Unlock()
	return res.ID, nil
}

func (a *Adobe) accountIDFor(ctx context.Context) (string, error) {
	a.mu.Lock()
	id := a.accountID
	a.mu.Unlock()
	if id != "" {
		return id, nil
	}
	acc, err := a.Account(ctx)
	return acc.ID, err
}

// catalogURL is {base}/v2/catalogs/{catalog_id}{suffix}.
func (a *Adobe) catalogURL(ctx context.Context, suffix string) (string, error) {
	cid, err := a.Catalog(ctx)
	if err != nil {
		return "", err
	}
	return a.baseURL + "/v2/catalogs/" + url.PathEscape(cid) + suffix, nil
}

// call is do on a catalogue path; a 404 for the catalogue drops the cached ID
// so the retry looks it up again.
func (a *Adobe) call(ctx context.Context, op, method, template, suffix, contentType string,
	body io.Reader, size int64, out any,
) (int, error) {
	u, err := a.catalogURL(ctx, suffix)
	if err != nil {
		return 0, err
	}
	status, err := a.do(ctx, op, method, template, u, contentType, body, size, out)
	var se *StatusError
	if errors.As(err, &se) && se.StatusCode == http.StatusNotFound && se.has("catalog", "does not exist") {
		a.mu.Lock()
		a.catalogID = ""
		a.mu.Unlock()
	}
	return status, err
}

func assetSuffix(id, rest string) string { return "/assets/" + url.PathEscape(id) + rest }

func (a *Adobe) CreateAsset(ctx context.Context, n NewAsset) (Asset, bool, error) {
	accountID, err := a.accountIDFor(ctx)
	if err != nil {
		return Asset{}, false, err
	}
	now := a.now().UTC().Format("2006-01-02T15:04:05Z")
	// The documented create body (quickstart/upload.md): the capture date is
	// filled in from the master, and importedOnDevice is the API key so
	// Lightroom shows the asset as coming from this integration
	// (quickstart/project.md).
	body := map[string]any{
		"subtype": "image",
		"payload": map[string]any{
			"captureDate": "0000-00-00T00:00:00",
			"userCreated": now,
			"userUpdated": now,
			"importSource": map[string]any{
				"fileName":         n.FileName,
				"importedOnDevice": a.apiKey,
				"importedBy":       accountID,
				"importTimestamp":  now,
			},
		},
	}
	data, err := json.Marshal(body)
	if err != nil {
		return Asset{}, false, err
	}
	_, err = a.call(ctx, "create_asset", http.MethodPut, assetTemplate, assetSuffix(n.ID, ""),
		"application/json", bytes.NewReader(data), int64(len(data)), nil)
	if err == nil {
		return Asset{ID: n.ID}, true, nil
	}
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusForbidden {
		return Asset{}, false, err
	}
	// 403 is also "already exists" (or quota, or a bad key): if the asset
	// is there, this is a replay.
	existing, found, gerr := a.getAsset(ctx, n.ID)
	if gerr != nil || !found {
		return Asset{}, false, err
	}
	return existing, false, nil
}

func (a *Adobe) getAsset(ctx context.Context, id string) (Asset, bool, error) {
	var res assetResource
	_, err := a.call(ctx, "get_asset", http.MethodGet, assetTemplate, assetSuffix(id, ""), "", nil, 0, &res)
	var se *StatusError
	if errors.As(err, &se) && se.StatusCode == http.StatusNotFound && !se.has("catalog", "does not exist") {
		return Asset{}, false, nil
	}
	if err != nil {
		return Asset{}, false, err
	}
	asset := res.asset()
	// Adobe reports importSource.sha256 once it has the original.
	asset.MasterSHA256 = asset.SHA256
	return asset, true, nil
}

func (a *Adobe) UploadMaster(ctx context.Context, assetID, contentType string, body io.Reader, size int64) (bool, error) {
	// The spec allows only image/jpeg for JPEGs; camera raws go up as
	// application/octet-stream.
	if contentType != "image/jpeg" {
		contentType = "application/octet-stream"
	}
	_, err := a.call(ctx, "upload_master", http.MethodPut, assetTemplate+"/master",
		assetSuffix(assetID, "/master"), contentType, body, size, nil)
	var se *StatusError
	if errors.As(err, &se) && se.StatusCode == http.StatusForbidden && alreadyExists(se) {
		return false, nil // the ID is derived from these bytes: it is our master
	}
	return err == nil, err
}

func alreadyExists(se *StatusError) bool {
	for _, msgs := range se.Errors {
		for _, m := range msgs {
			if m == "already exists" {
				return true
			}
		}
	}
	return false
}

// SetRating cannot be done through the Partner API: assets cannot be
// updated after create. The rating stays in Keeper.
func (a *Adobe) SetRating(context.Context, string, int) error { return ErrRatingUnsupported }

const (
	albumsTemplate      = "/v2/catalogs/{catalog_id}/albums"
	albumAssetsTemplate = "/v2/catalogs/{catalog_id}/albums/{album_id}/assets"
	// maxPages stops a server that keeps handing out next links.
	maxPages = 200
	// AlbumBatch is how many assets one add-to-album call takes.
	AlbumBatch = 50
)

type albumResource struct {
	ID      string `json:"id"`
	Subtype string `json:"subtype"`
	Payload struct {
		Name   string `json:"name"`
		Parent *struct {
			ID string `json:"id"`
		} `json:"parent"`
	} `json:"payload"`
}

type page struct {
	Base  string `json:"base"`
	Links struct {
		Next *struct {
			Href string `json:"href"`
		} `json:"next"`
	} `json:"links"`
}

// ListCollections reads all collection sets and collections and returns the
// collections with their paths, sorted by path.
func (a *Adobe) ListCollections(ctx context.Context) ([]Collection, error) {
	sets, err := a.listAlbums(ctx, "collection_set")
	if err != nil {
		return nil, err
	}
	cols, err := a.listAlbums(ctx, "collection")
	if err != nil {
		return nil, err
	}
	byID := make(map[string]albumResource, len(sets))
	for _, s := range sets {
		byID[s.ID] = s
	}
	out := make([]Collection, 0, len(cols))
	for _, c := range cols {
		if c.Subtype != "" && c.Subtype != "collection" {
			continue
		}
		names := []string{c.Payload.Name}
		seen := map[string]bool{c.ID: true}
		for p := c.Payload.Parent; p != nil && p.ID != "" && !seen[p.ID]; {
			set, ok := byID[p.ID]
			if !ok {
				break
			}
			seen[p.ID] = true
			names = append(names, set.Payload.Name)
			p = set.Payload.Parent
		}
		for i, j := 0, len(names)-1; i < j; i, j = i+1, j-1 {
			names[i], names[j] = names[j], names[i]
		}
		out = append(out, Collection{ID: c.ID, Name: c.Payload.Name, Path: strings.Join(names, " / ")})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return strings.ToLower(out[i].Path) < strings.ToLower(out[j].Path)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// listAlbums pages through GET albums?subtype=… by following links.next
// (relative to "base"), never off the API host.
func (a *Adobe) listAlbums(ctx context.Context, subtype string) ([]albumResource, error) {
	first, err := a.catalogURL(ctx, "/albums?subtype="+url.QueryEscape(subtype))
	if err != nil {
		return nil, err
	}
	api, err := url.Parse(a.baseURL)
	if err != nil {
		return nil, err
	}
	var all []albumResource
	seen := map[string]bool{}
	for u := first; u != ""; {
		if seen[u] || len(seen) >= maxPages {
			return nil, &StatusError{Op: "list_albums", StatusCode: http.StatusBadGateway, Description: "album paging does not end"}
		}
		seen[u] = true
		var res struct {
			page
			Resources []albumResource `json:"resources"`
		}
		if _, err := a.do(ctx, "list_albums", http.MethodGet, albumsTemplate, u, "", nil, 0, &res); err != nil {
			return nil, err
		}
		all = append(all, res.Resources...)
		u = ""
		if res.Links.Next != nil && res.Links.Next.Href != "" {
			next, err := resolveNext(api, res.Base, res.Links.Next.Href)
			if err != nil {
				return nil, err
			}
			u = next
		}
	}
	return all, nil
}

// resolveNext resolves a links.next href against the page's base (or the
// API root) and refuses to leave the API's scheme and host: the request
// carries the user's token.
func resolveNext(api *url.URL, base, href string) (string, error) {
	b := api
	if base != "" {
		pb, err := url.Parse(base)
		if err != nil {
			return "", err
		}
		b = api.ResolveReference(pb)
	}
	h, err := url.Parse(href)
	if err != nil {
		return "", err
	}
	next := b.ResolveReference(h)
	if next.Scheme != api.Scheme || next.Host != api.Host {
		return "", &StatusError{Op: "list_albums", StatusCode: http.StatusBadGateway,
			Description: "next link leaves " + api.Host}
	}
	return next.String(), nil
}

// AddToAlbum adds assets in batches of AlbumBatch. Adobe answers 201 when
// at least one asset was added and 403 when none was, with a per-asset error
// list; an asset that was already in the album is one of those errors. So
// every asset Adobe did not add is looked up in the album: present counts as
// added, missing is an *AlbumRefusedError.
func (a *Adobe) AddToAlbum(ctx context.Context, albumID string, assetIDs []string) error {
	for start := 0; start < len(assetIDs); start += AlbumBatch {
		if err := a.addBatch(ctx, albumID, assetIDs[start:min(start+AlbumBatch, len(assetIDs))]); err != nil {
			return err
		}
	}
	return nil
}

type albumAddError struct {
	ID          string              `json:"id"`
	HTTPStatus  int                 `json:"http_status"`
	Description string              `json:"description"`
	Errors      map[string][]string `json:"errors"`
}

func (a *Adobe) addBatch(ctx context.Context, albumID string, ids []string) error {
	type item struct {
		ID      string         `json:"id"`
		Payload map[string]any `json:"payload"`
	}
	req := struct {
		Resources []item `json:"resources"`
	}{}
	for _, id := range ids {
		req.Resources = append(req.Resources, item{ID: id, Payload: map[string]any{}})
	}
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	var res struct {
		Errors []albumAddError `json:"errors"`
	}
	suffix := "/albums/" + url.PathEscape(albumID) + "/assets"
	status, err := a.call(ctx, "add_to_album", http.MethodPut, albumAssetsTemplate, suffix,
		"application/json", bytes.NewReader(data), int64(len(data)), &res)

	var failed []albumAddError
	var se *StatusError
	switch {
	case err == nil:
		failed = res.Errors // 201 with some assets not added
	case errors.As(err, &se) && se.StatusCode == http.StatusForbidden && se.Code != "4300" && se.Code != "403003":
		// None added. The per-asset list says why; without it, all failed.
		var body struct {
			Errors []albumAddError `json:"errors"`
		}
		_ = json.Unmarshal(se.body, &body)
		failed = body.Errors
		if len(failed) == 0 {
			for _, id := range ids {
				failed = append(failed, albumAddError{ID: id, HTTPStatus: status, Description: se.Description})
			}
		}
	default:
		if se, ok := albumGone(err); ok && se.StatusCode == http.StatusNotFound {
			return &AlbumRefusedError{AlbumID: albumID, AssetIDs: ids, StatusCode: se.StatusCode,
				Description: "collection not found"}
		}
		return err // auth, 5xx, timeouts: Classify decides
	}
	if len(failed) == 0 {
		return nil
	}

	want := make([]string, 0, len(failed))
	for _, f := range failed {
		want = append(want, f.ID)
	}
	present, err := a.inAlbum(ctx, albumID, want)
	if err != nil {
		if se, ok := albumGone(err); ok {
			// Not even readable: the album is not ours to fill.
			return &AlbumRefusedError{AlbumID: albumID, AssetIDs: want, StatusCode: se.StatusCode, Description: se.Description}
		}
		return err
	}
	refused := &AlbumRefusedError{AlbumID: albumID, StatusCode: http.StatusForbidden}
	for _, f := range failed {
		if present[f.ID] {
			continue
		}
		refused.AssetIDs = append(refused.AssetIDs, f.ID)
		if refused.Description == "" {
			refused.Description = f.reason()
		}
		if f.HTTPStatus != 0 {
			refused.StatusCode = f.HTTPStatus
		}
	}
	if len(refused.AssetIDs) == 0 {
		return nil
	}
	return refused
}

// albumGone is a 403 that is not about the token or key, or a 404 that is
// not about the catalogue: the album itself is out of reach.
func albumGone(err error) (*StatusError, bool) {
	var se *StatusError
	if !errors.As(err, &se) {
		return nil, false
	}
	switch {
	case se.StatusCode == http.StatusForbidden && se.Code != "4300" && se.Code != "403003":
		return se, true
	case se.StatusCode == http.StatusNotFound && !se.has("catalog", "does not exist"):
		return se, true
	}
	return nil, false
}

func (e albumAddError) reason() string {
	var parts []string
	if e.Description != "" {
		parts = append(parts, e.Description)
	}
	keys := make([]string, 0, len(e.Errors))
	for k := range e.Errors {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, k+": "+strings.Join(e.Errors[k], ", "))
	}
	return strings.Join(parts, "; ")
}

// inAlbum reports which of ids are in the album
// (GET …/albums/{id}/assets?asset_ids=…, at most 100 per call).
func (a *Adobe) inAlbum(ctx context.Context, albumID string, ids []string) (map[string]bool, error) {
	present := map[string]bool{}
	for start := 0; start < len(ids); start += 100 {
		chunk := ids[start:min(start+100, len(ids))]
		var res struct {
			Resources []struct {
				Asset struct {
					ID string `json:"id"`
				} `json:"asset"`
			} `json:"resources"`
		}
		suffix := "/albums/" + url.PathEscape(albumID) + "/assets?asset_ids=" + url.QueryEscape(strings.Join(chunk, ","))
		if _, err := a.call(ctx, "list_album_assets", http.MethodGet, albumAssetsTemplate, suffix, "", nil, 0, &res); err != nil {
			return nil, err
		}
		for _, r := range res.Resources {
			present[r.Asset.ID] = true
		}
	}
	return present, nil
}

// SetUploadTimeout bounds each master upload separately from the other
// calls (0: the call timeout).
func (a *Adobe) SetUploadTimeout(d time.Duration) { a.uploadTimeout = d }

// Reset forgets the cached catalogue and account IDs, e.g. after a
// different Adobe user connected.
func (a *Adobe) Reset() {
	a.mu.Lock()
	a.catalogID, a.accountID = "", ""
	a.mu.Unlock()
}

// String is for logs.
func (a *Adobe) String() string { return fmt.Sprintf("adobe(%s)", a.baseURL) }
