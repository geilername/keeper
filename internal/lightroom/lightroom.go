// Package lightroom is Keeper's port to a Lightroom catalogue. Client is the
// interface the publisher depends on; HTTP implements it for the
// Adobe-shaped API of cmd/lightroom-mock, so a real Adobe adapter (OAuth,
// renditions, Adobe's rating shape) can be slotted in behind the same
// interface later.
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
)

// StatusError is a non-2xx answer from Lightroom.
type StatusError struct {
	Op          string // e.g. "create_asset"
	StatusCode  int
	Description string // Lightroom's error description, if any
}

func (e *StatusError) Error() string {
	if e.Description == "" {
		return fmt.Sprintf("lightroom %s: HTTP %d", e.Op, e.StatusCode)
	}
	return fmt.Sprintf("lightroom %s: HTTP %d: %s", e.Op, e.StatusCode, e.Description)
}

// Classify says what kind of failure err is and whether trying again later
// can help. Only errors returned by a Client are expected here.
func Classify(err error) (class string, retryable bool) {
	var se *StatusError
	var ne net.Error
	switch {
	case errors.As(err, &se):
		switch {
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
