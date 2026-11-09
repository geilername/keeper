// Package policy decides which frames of a burst to keep. It is the one fixed
// culling policy (HACKATHON-PLAN §2): pure functions over scorer features, no
// NATS, no OpenTelemetry, so every rule is covered by table tests.
//
//	best   5★  the sharpest frame of the burst that passes every check
//	keeper 3★  every other frame that passes every check
//	reject 1★  a frame that fails a check: eyes closed, blurred, badly exposed
//
// A frame passes when its eyes are not closed, it is sharp (absolutely, and
// compared with the sharpest frame of its burst) and it is well exposed. If no
// frame of a burst passes, the burst has no best frame and every frame is a
// reject.
package policy

import (
	"cmp"
	"fmt"
	"time"

	"github.com/geilername/keeper/internal/events"
)

// Thresholds. Scores come from the scorer and are 0..1 (scorer/features.py).
const (
	// MinSharpness is the absolute floor. The scorer maps a Laplacian
	// variance of 100 (the usual "blurry below this" rule) to 0.5.
	MinSharpness = 0.5

	// MinRelativeSharpness: inside a burst, a frame below this fraction of
	// the burst's sharpest frame is blurred, even if it clears MinSharpness.
	// Burst frames show the same scene, so their sharpness is comparable.
	MinRelativeSharpness = 0.6

	// MinExposure is the floor for the scorer's exposure score (1.0 = mean
	// at mid-grey and nothing clipped).
	MinExposure = 0.35
)

// Star ratings per bucket.
const (
	RatingBest   = 5
	RatingKeeper = 3
	RatingReject = 1
)

// Frame is one scored photo of a burst.
type Frame struct {
	SHA256     string
	CapturedAt time.Time
	Features   events.Features
}

// Decision is the outcome for one frame.
type Decision struct {
	SHA256  string
	Bucket  string // events.Bucket*
	Rating  int
	Reasons []string // shown to the photographer, one short phrase each
}

// Result is the outcome for a whole burst.
type Result struct {
	Best      string     // sha256 of the best frame, "" if no frame passed
	Reason    string     // why the best frame won, or why there is none
	Decisions []Decision // same order as the input frames
}

// Decide applies the policy to one burst. frames must not be empty.
func Decide(frames []Frame) Result {
	sharpest := 0.0
	for _, f := range frames {
		sharpest = max(sharpest, f.Features.Sharpness.Score)
	}

	// Check every frame; the best frame is the sharpest one that passes.
	failures := make([][]string, len(frames))
	best := -1
	for i, f := range frames {
		failures[i] = check(f, sharpest, len(frames))
		if len(failures[i]) == 0 && (best < 0 || better(f, frames[best])) {
			best = i
		}
	}

	res := Result{Decisions: make([]Decision, len(frames))}
	for i, f := range frames {
		d := Decision{SHA256: f.SHA256}
		switch {
		case len(failures[i]) > 0:
			d.Bucket, d.Rating, d.Reasons = events.BucketReject, RatingReject, failures[i]
		case i == best:
			d.Bucket, d.Rating, d.Reasons = events.BucketBest, RatingBest, []string{bestReason(f, len(frames))}
		default:
			d.Bucket, d.Rating = events.BucketKeeper, RatingKeeper
			d.Reasons = []string{fmt.Sprintf("sharp and well exposed, second to the best frame (sharpness %.2f vs %.2f)",
				f.Features.Sharpness.Score, frames[best].Features.Sharpness.Score)}
		}
		res.Decisions[i] = d
	}

	if best >= 0 {
		res.Best = frames[best].SHA256
		res.Reason = res.Decisions[best].Reasons[0]
	} else {
		res.Reason = "no frame passed the checks"
	}
	return res
}

// check returns the reasons a frame fails, in the order a photographer
// would look: eyes, focus, light. No reasons means the frame passes.
func check(f Frame, sharpest float64, burstSize int) []string {
	var reasons []string
	if f.Features.Eyes.State == events.EyesClosed {
		reasons = append(reasons, "eyes closed")
	}

	s := f.Features.Sharpness.Score
	switch {
	case burstSize > 1 && s < MinRelativeSharpness*sharpest:
		reasons = append(reasons, fmt.Sprintf("motion blur (sharpness %.2f, sharpest in burst %.2f)", s, sharpest))
	case s < MinSharpness:
		reasons = append(reasons, fmt.Sprintf("blurred (sharpness %.2f)", s))
	}

	if e := f.Features.Exposure; e.Score < MinExposure {
		side := "overexposed"
		if e.Mean < 0.5 {
			side = "underexposed"
		}
		reasons = append(reasons, fmt.Sprintf("%s (exposure %.2f, mean brightness %.2f)", side, e.Score, e.Mean))
	}
	return reasons
}

// better orders passing frames: sharper wins, then better exposed, then
// earlier, then by sha256 so the result never depends on input order.
func better(a, b Frame) bool {
	if c := cmp.Compare(a.Features.Sharpness.Score, b.Features.Sharpness.Score); c != 0 {
		return c > 0
	}
	if c := cmp.Compare(a.Features.Exposure.Score, b.Features.Exposure.Score); c != 0 {
		return c > 0
	}
	if !a.CapturedAt.Equal(b.CapturedAt) {
		return a.CapturedAt.Before(b.CapturedAt)
	}
	return a.SHA256 < b.SHA256
}

func bestReason(f Frame, burstSize int) string {
	eyes := ""
	if f.Features.Eyes.State == events.EyesOpen {
		eyes = " with eyes open"
	}
	if burstSize == 1 {
		return fmt.Sprintf("single frame, sharp and well exposed%s (sharpness %.2f)", eyes, f.Features.Sharpness.Score)
	}
	return fmt.Sprintf("sharpest frame%s (sharpness %.2f)", eyes, f.Features.Sharpness.Score)
}
