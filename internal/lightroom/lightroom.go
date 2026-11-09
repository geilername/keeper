// Package lightroom is Keeper's port to a Lightroom catalogue. Client is the
// interface the publisher depends on. HTTP implements it for the
// Adobe-shaped API of cmd/lightroom-mock; Adobe implements it, plus Albums,
// for the real Lightroom Partner API at lr.adobe.io, and IMS is the Adobe
// OAuth client that gets the user's tokens.
//
// Every operation is idempotent so a redelivered message can replay all of
// them (ADR-2): the asset ID is derived from the photo's sha256, create is a
// PUT on that ID, the master upload is accepted again for the same bytes,
// and the rating is a plain set.
package lightroom

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
)

// DefaultCatalogID is the mock's fixed catalogue ID.
const DefaultCatalogID = "00000000000000000000000000000001"

// Client is what the publisher needs from Lightroom.
type Client interface {
	// CreateAsset creates the asset with the given ID, or returns the
	// existing one if the ID is already known (created == false).
	CreateAsset(ctx context.Context, a NewAsset) (asset Asset, created bool, err error)
	// UploadMaster uploads the original bytes. Uploading the same bytes
	// again succeeds (created == false).
	UploadMaster(ctx context.Context, assetID, contentType string, body io.Reader, size int64) (created bool, err error)
	// SetRating sets the star rating (1..5).
	SetRating(ctx context.Context, assetID string, rating int) error
}

// AssetID maps a photo's hex sha256 to its Lightroom asset ID: the first 32
// hex characters. Lightroom asset IDs are client-chosen 32-hex strings, so a
// content-derived ID turns "create" into an idempotent PUT.
func AssetID(sha256Hex string) string {
	if len(sha256Hex) < 32 {
		return sha256Hex
	}
	return sha256Hex[:32]
}

// NewAsset is the request to create an image asset.
type NewAsset struct {
	ID       string
	FileName string
	SHA256   string // of the original, hex
}

// Asset is the part of the Lightroom asset resource Keeper reads.
type Asset struct {
	ID     string
	SHA256 string // importSource.sha256
	// MasterSHA256 is the sha256 of the uploaded original, if the backend
	// reports one (the mock does); empty means "unknown or not uploaded".
	MasterSHA256 string
}

// Error classes, used as the error.type span attribute and the
// error_class field of photo.failed. Keep the set small: it is also a
// metric-friendly label.
const (
	ClassServer      = "lightroom_5xx" // 5xx: retry later
	ClassThrottled   = "lightroom_429" // 429: retry later
	ClassTimeout     = "timeout"       // no response within the call timeout
	ClassUnreachable = "unreachable"   // connection refused, DNS, reset, ...
	ClassConflict    = "conflict"      // 409/412: ID or bytes clash, retry cannot fix it
	ClassClient      = "lightroom_4xx" // any other 4xx: our request is wrong
	ClassCanceled    = "canceled"      // our own context ended (shutdown)
	// Adobe only.
	ClassAuth     = "lightroom_auth"    // no valid user token (not connected, expired): retry later
	ClassCatalog  = "lightroom_catalog" // the user's catalogue ID changed: retry with the new one
	ClassNoTarget = "no_target"         // no collection chosen yet: hold and retry
	ClassRefused  = "album_refused"     // Lightroom will not add the asset to the collection
)

// ErrNotConnected is returned by a TokenSource when there is no usable
// Lightroom access token (nobody connected, or the token expired and could
// not be refreshed). It is retryable: connecting in the console heals it.
var ErrNotConnected = errors.New("lightroom: not connected")

// ErrRatingUnsupported is returned by SetRating when the backend has no way
// to write a rating. The Lightroom Partner API cannot change an asset after
// it is created (no PATCH, no rating field on create), so Adobe returns it;
// the publisher records that the rating stayed in Keeper and goes on.
var ErrRatingUnsupported = errors.New("lightroom: this API cannot write ratings")

// StatusError is a non-2xx answer from Lightroom.
type StatusError struct {
	Op          string // e.g. "create_asset"
	StatusCode  int
	Code        string              // Adobe's error code ("4300", "403003"), if any
	Description string              // Lightroom's error description, if any
	Errors      map[string][]string // Adobe's per-field errors, e.g. {"asset": ["already exists"]}

	body []byte // the error body, prefix stripped, for callers that need more
}

// has reports whether Adobe listed msg for field (e.g. "asset", "already exists").
func (e *StatusError) has(field, msg string) bool {
	for _, m := range e.Errors[field] {
		if m == msg {
			return true
		}
	}
	return false
}

// AlbumRefusedError says Lightroom would not put the assets into the album
// even though the request itself was fine: the album does not accept them
// (Adobe answers 403 with a per-asset error). Retrying cannot help.
type AlbumRefusedError struct {
	AlbumID     string
	AssetIDs    []string // the ones that are not in the album
	StatusCode  int
	Description string // Adobe's per-asset reason, if it gave one
}

func (e *AlbumRefusedError) Error() string {
	s := fmt.Sprintf("lightroom refused album %s for %d asset(s): HTTP %d", e.AlbumID, len(e.AssetIDs), e.StatusCode)
	if e.Description != "" {
		s += ": " + e.Description
	}
	return s
}

func (e *StatusError) Error() string {
	if e.Description == "" {
		return fmt.Sprintf("lightroom %s: HTTP %d", e.Op, e.StatusCode)
	}
	return fmt.Sprintf("lightroom %s: HTTP %d: %s", e.Op, e.StatusCode, e.Description)
}

// Albums is what Keeper needs from a backend that has albums (Adobe):
// finding the user's collections and adding assets to one of them.
type Albums interface {
	// ListCollections returns every collection of the catalogue, with
	// its path through the collection sets it is nested in.
	ListCollections(ctx context.Context) ([]Collection, error)
	// AddToAlbum adds the assets to the album. An asset that is already in
	// the album counts as added. *AlbumRefusedError when Lightroom will not
	// take them.
	AddToAlbum(ctx context.Context, albumID string, assetIDs []string) error
}

// Collection is a user's Lightroom collection (an album of subtype
// "collection").
type Collection struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Path is the names of the enclosing collection sets and the
	// collection, joined by " / ", e.g. "2026 / Conferences / KubeCon".
	Path string `json:"path"`
}

// TokenSource hands out a valid Lightroom user access token, or
// ErrNotConnected.
type TokenSource interface {
	AccessToken(ctx context.Context) (string, error)
}

// StaticToken is a TokenSource for one token, e.g. right after the OAuth
// code exchange.
type StaticToken string

func (t StaticToken) AccessToken(context.Context) (string, error) {
	if t == "" {
		return "", ErrNotConnected
	}
	return string(t), nil
}

// Classify says what kind of failure err is and whether trying again later
// can help. Only errors returned by a Client are expected here.
func Classify(err error) (class string, retryable bool) {
	var se *StatusError
	var ne net.Error
	var refused *AlbumRefusedError
	switch {
	case errors.As(err, &refused):
		return ClassRefused, false
	case errors.Is(err, ErrNotConnected):
		return ClassAuth, true
	case errors.As(err, &se):
		switch {
		case se.StatusCode == http.StatusUnauthorized,
			// Adobe answers an expired access token with 403 code 4300.
			se.StatusCode == http.StatusForbidden && se.Code == "4300":
			return ClassAuth, true
		case se.StatusCode == http.StatusNotFound && se.has("catalog", "does not exist"):
			return ClassCatalog, true
		case se.StatusCode >= 500:
			return ClassServer, true
		case se.StatusCode == http.StatusTooManyRequests:
			return ClassThrottled, true
		case se.StatusCode == http.StatusConflict, se.StatusCode == http.StatusPreconditionFailed:
			return ClassConflict, false
		default:
			return ClassClient, false
		}
	case errors.Is(err, context.Canceled):
		return ClassCanceled, true
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return ClassTimeout, true
	default:
		// No HTTP response at all: refused, DNS failure, connection reset.
		return ClassUnreachable, true
	}
}
