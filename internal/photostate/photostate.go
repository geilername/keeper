// Package photostate is the fold from PHOTOS stream events to the current
// state of one photo. The gateway's Postgres read model and the indexer's
// Elasticsearch index both apply it, so they agree on every field.
package photostate

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/geilername/keeper/internal/events"
)

// Stage is where a photo is in the pipeline, as the console shows it.
type Stage string

const (
	StageNone       Stage = "" // row exists, no stage-setting event applied yet
	StageIngested   Stage = "ingested"
	StageScored     Stage = "scored"
	StageDecided    Stage = "decided"
	StagePublishing Stage = "publishing" // reserved: no event in §3.3 sets it yet
	StageRetrying   Stage = "retrying"
	StageFailed     Stage = "failed"
	StagePublished  Stage = "published"
)

// rank orders the stages. A photo only ever moves to a stage of higher rank,
// so redelivered or late events cannot move it backwards.
//
// failed ranks above retrying (the final failure comes after the last retry)
// but below published: if Lightroom has the photo, that is the truth, even
// when an older failure arrives later.
var rank = map[Stage]int{
	StageNone:       0,
	StageIngested:   1,
	StageScored:     2,
	StageDecided:    3,
	StagePublishing: 4,
	StageRetrying:   5,
	StageFailed:     6,
	StagePublished:  7,
}

// advance returns the stage a photo is in after an event that implies next.
func advance(cur, next Stage) Stage {
	if rank[next] > rank[cur] {
		return next
	}
	return cur
}

// Photo is the state of one photo: a row of gateway.photos and a document
// of the Elasticsearch photos index. Times are UTC and truncated to
// microseconds, the precision Postgres stores, so a photo read back from the
// database compares equal to the one that was written.
type Photo struct {
	SHA256           string           `json:"sha256"`
	FileName         string           `json:"file_name,omitempty"`
	CapturedAt       *time.Time       `json:"captured_at,omitempty"`
	Stage            Stage            `json:"stage"`
	Features         *events.Features `json:"features,omitempty"`
	BurstID          string           `json:"burst_id,omitempty"`
	Bucket           string           `json:"bucket,omitempty"`
	Rating           int              `json:"rating,omitempty"`
	Reasons          []string         `json:"reasons"`
	Attempts         int              `json:"attempts"`
	LastError        string           `json:"last_error,omitempty"`
	LightroomAssetID string           `json:"lightroom_asset_id,omitempty"`
	SeenAt           time.Time        `json:"seen_at"`
	IngestedAt       *time.Time       `json:"ingested_at,omitempty"`
	ScoredAt         *time.Time       `json:"scored_at,omitempty"`
	DecidedAt        *time.Time       `json:"decided_at,omitempty"`
	PublishedAt      *time.Time       `json:"published_at,omitempty"`
	FailedAt         *time.Time       `json:"failed_at,omitempty"`
	RetryAt          *time.Time       `json:"retry_at,omitempty"` // next publish attempt while retrying
	UpdatedAt        time.Time        `json:"updated_at"`
}

// Apply folds one event into p. It is pure and idempotent: applying the same
// event twice, or events in a different order, gives the same photo, except
// that the stage only moves forward.
//
// at is the event's stream timestamp. firstPublished is true only for the
// event that moves the photo into published for the first time; the e2e
// metric is recorded exactly then.
func Apply(p Photo, ev any, at time.Time) (out Photo, firstPublished bool) {
	at = NormTime(at)
	p.SeenAt = earliest(p.SeenAt, at)

	switch e := ev.(type) {
	case *events.PhotoIngested:
		p.FileName = e.FileName
		if !e.CapturedAt.IsZero() {
			p.CapturedAt = ptr(NormTime(e.CapturedAt))
		}
		p.IngestedAt = earliestPtr(p.IngestedAt, at)
		p.Stage = advance(p.Stage, StageIngested)

	case *events.PhotoScored:
		f := e.Features
		p.Features = &f
		if p.CapturedAt == nil && !e.CapturedAt.IsZero() {
			p.CapturedAt = ptr(NormTime(e.CapturedAt))
		}
		p.ScoredAt = earliestPtr(p.ScoredAt, at)
		p.Stage = advance(p.Stage, StageScored)

	case *events.PhotoDecided:
		p.BurstID = e.BurstID
		p.Bucket = e.Bucket
		p.Rating = e.Rating
		p.Reasons = e.Reasons
		p.DecidedAt = earliestPtr(p.DecidedAt, at)
		p.Stage = advance(p.Stage, StageDecided)

	case *events.PhotoPublished:
		firstPublished = p.PublishedAt == nil
		p.LightroomAssetID = e.LightroomAssetID
		p.Attempts = max(p.Attempts, e.Attempt)
		p.PublishedAt = earliestPtr(p.PublishedAt, at)
		p.Stage = advance(p.Stage, StagePublished)
		p.RetryAt = nil

	case *events.PhotoFailed:
		// Only the newest attempt describes the current error; the same
		// attempt seen again keeps its first time.
		switch {
		case e.Attempt > p.Attempts:
			p.Attempts = e.Attempt
			p.LastError = e.ErrorClass
			p.FailedAt = ptr(at)
			p.RetryAt = e.RetryAt
		case e.Attempt == p.Attempts:
			p.LastError = e.ErrorClass
			p.FailedAt = earliestPtr(p.FailedAt, at)
		}
		// retry_at set: the publisher will try again. Absent: it gave up.
		next := StageFailed
		if e.RetryAt != nil {
			next = StageRetrying
		}
		p.Stage = advance(p.Stage, next)
		if p.Stage == StagePublished {
			p.RetryAt = nil // a late failure never brings a retry time back
		}
	}
	if p.Reasons == nil {
		p.Reasons = []string{}
	}
	return p, firstPublished
}

func NormTime(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

func ptr[T any](v T) *T { return &v }

func earliest(cur, t time.Time) time.Time {
	if cur.IsZero() || t.Before(cur) {
		return t
	}
	return cur
}

func earliestPtr(cur *time.Time, t time.Time) *time.Time {
	if cur == nil || t.Before(*cur) {
		return ptr(t)
	}
	return cur
}

// Decode decodes a photo.* payload into its contract type. It returns a nil
// event for subjects it does not know.
func Decode(subject string, raw []byte) (string, any, error) {
	var (
		ev  any
		sha *string
	)
	switch subject {
	case events.SubjectIngested:
		e := &events.PhotoIngested{}
		ev, sha = e, &e.SHA256
	case events.SubjectScored:
		e := &events.PhotoScored{}
		ev, sha = e, &e.SHA256
	case events.SubjectDecided:
		e := &events.PhotoDecided{}
		ev, sha = e, &e.SHA256
	case events.SubjectPublished:
		e := &events.PhotoPublished{}
		ev, sha = e, &e.SHA256
	case events.SubjectFailed:
		e := &events.PhotoFailed{}
		ev, sha = e, &e.SHA256
	default:
		return "", nil, nil
	}
	if err := json.Unmarshal(raw, ev); err != nil {
		return "", nil, fmt.Errorf("decode %s: %w", subject, err)
	}
	if !ValidSHA(*sha) {
		return "", nil, fmt.Errorf("%s: invalid sha256 %q", subject, *sha)
	}
	return *sha, ev, nil
}

// ValidSHA accepts exactly 64 lowercase hex characters. Everything that turns
// a URL segment into a file path, a query or a document ID goes through it.
func ValidSHA(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
