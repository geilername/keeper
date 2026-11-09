// Package events holds the versioned JSON contracts that travel on the
// PHOTOS JetStream stream (HACKATHON-PLAN §3.3). Messages carry references
// (sha256, spool path), never photo bytes (ADR-3).
//
// The Python scorer mirrors PhotoScored in scorer/src/keeper_scorer; keep
// both in sync.
package events

import "time"

// Version is the contract version every event carries in "v".
const Version = 1

// Stream and subjects.
const (
	Stream = "PHOTOS"

	SubjectIngested  = "photo.ingested"
	SubjectScored    = "photo.scored"
	SubjectDecided   = "photo.decided"
	SubjectPublished = "photo.published"
	SubjectFailed    = "photo.failed"
	SubjectBurst     = "burst.decided"
)

// StreamSubjects are the subjects the PHOTOS stream captures.
var StreamSubjects = []string{"photo.>", "burst.>"}

// MsgID is the JetStream dedup ID for an event about one photo or burst
// (ADR-2): publishing the same event twice inside the dedup window is a
// no-op.
func MsgID(subject, id string) string { return subject + ":" + id }

type PhotoIngested struct {
	V        int    `json:"v"`
	SHA256   string `json:"sha256"` // of the original as shot
	FileName string `json:"file_name"`
	// Path is the upright JPEG to analyse: the file itself for a JPEG, the
	// embedded preview for a raw. OriginalPath is the file as shot.
	Path         string    `json:"path"`
	OriginalPath string    `json:"original_path"`
	MediaType    string    `json:"media_type"` // image/jpeg | image/x-sony-arw
	SizeBytes    int64     `json:"size_bytes"` // of the original
	CapturedAt   time.Time `json:"captured_at"`
	CameraModel  string    `json:"camera_model,omitempty"`
}

type PhotoScored struct {
	V          int                `json:"v"`
	SHA256     string             `json:"sha256"`
	FileName   string             `json:"file_name,omitempty"` // passed through for Lightroom
	CapturedAt time.Time          `json:"captured_at"`
	Features   Features           `json:"features"`
	TimingsMS  map[string]float64 `json:"timings_ms"`
}

type Features struct {
	Sharpness Sharpness `json:"sharpness"`
	Exposure  Exposure  `json:"exposure"`
	Eyes      Eyes      `json:"eyes"`
	DHash     string    `json:"dhash"` // 64-bit difference hash, 16 hex chars
}

type Sharpness struct {
	Variance float64 `json:"variance"` // variance of the Laplacian
	Score    float64 `json:"score"`    // 0..1
}

type Exposure struct {
	Mean              float64 `json:"mean"` // 0..1 luminance
	HighlightsClipped float64 `json:"highlights_clipped"`
	ShadowsClipped    float64 `json:"shadows_clipped"`
	Score             float64 `json:"score"` // 0..1
}

// Eye states reported by the scorer.
const (
	EyesOpen   = "open"
	EyesClosed = "closed"
	EyesNoFace = "no_face"
)

type Eyes struct {
	State      string  `json:"state"`
	Confidence float64 `json:"confidence"`
	Faces      int     `json:"faces"`
}

// Buckets and the star rating each one maps to.
const (
	BucketBest   = "best"   // 5★
	BucketKeeper = "keeper" // 3★
	BucketReject = "reject" // 1★
)

type BurstDecided struct {
	V           int      `json:"v"`
	BurstID     string   `json:"burst_id"`
	Members     []string `json:"members"` // sha256, capture order
	Best        string   `json:"best"`
	Reason      string   `json:"reason"`
	CloseReason string   `json:"close_reason"` // "idle" | "max_wait"
}

type PhotoDecided struct {
	V        int      `json:"v"`
	SHA256   string   `json:"sha256"`
	FileName string   `json:"file_name,omitempty"` // the camera's name, used in Lightroom
	BurstID  string   `json:"burst_id"`
	Bucket   string   `json:"bucket"`
	Rating   int      `json:"rating"`
	Reasons  []string `json:"reasons"` // human-readable, shown on the photo sheet
}

type PhotoPublished struct {
	V                int    `json:"v"`
	SHA256           string `json:"sha256"`
	LightroomAssetID string `json:"lightroom_asset_id"`
	Attempt          int    `json:"attempt"`
}

type PhotoFailed struct {
	V          int    `json:"v"`
	SHA256     string `json:"sha256"`
	Attempt    int    `json:"attempt"`
	ErrorClass string `json:"error_class"` // e.g. "lightroom_5xx", "timeout"
	// Error says in words what the console should show instead of the
	// class, when there is something to say ("Lightroom refused collection
	// Talks"). Optional; readers fall back to error_class.
	Error   string     `json:"error,omitempty"`
	RetryAt *time.Time `json:"retry_at,omitempty"`
}
