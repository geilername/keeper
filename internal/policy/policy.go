// Package policy decides which frames of a burst to keep. Pure functions over
// scorer features, no NATS, no OpenTelemetry, so every rule is covered by
// table tests.
//
// There is one profile: conferences. Conference photos are taken in dim halls
// under stage and screen light, of people who talk, look at phones and turn
// away. A moment does not repeat, so the policy judges every frame against
// the other frames of its own burst, never against a studio ideal:
//
//	best   5★  the strongest usable frame of the burst; every burst keeps one
//	keeper 3★  a usable frame as good as the rest of its burst
//	reject 1★  unusable (nearly black, blown out, far too blurred), or clearly
//	           worse than its siblings: blurred next to them, a stop or more
//	           off their exposure, eyes closed where another frame has them open
//
// Darkness that the whole burst shares is the venue, not a fault; downcast
// eyes that the whole burst shares are a moment, not a blink.
package policy

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/geilername/keeper/internal/events"
)

// Profile names the policy in traces and logs.
const Profile = "conference"

// Thresholds. Scores come from the scorer and are 0..1 (scorer/features.py).
const (
	// UnusableSharpness: below this nothing is recognisable. The scorer maps
	// a Laplacian variance of 100 to 0.5; 0.25 is a variance of about 33.
	// High-ISO venue shots sit well above it.
	UnusableSharpness = 0.25

	// NearlyBlack and BlownOut bound the mean luminance (0..1) of a usable
	// frame; BlownOutClipped is the share of clipped highlights that makes
	// a frame unusable even with a normal mean.
	NearlyBlack     = 0.04
	BlownOut        = 0.96
	BlownOutClipped = 0.40

	// MinRelativeSharpness: inside a burst, a frame below this fraction of
	// the sharpest usable frame is blurred. Burst frames show the same
	// scene, so their sharpness is comparable.
	MinRelativeSharpness = 0.6

	// MaxExposureOffEV: a frame further than this from the burst's median
	// exposure (in stops) is badly exposed for that scene.
	MaxExposureOffEV = 1.0
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
	Best      string     // sha256 of the best frame, "" if no frame is usable
	Reason    string     // why the best frame won, or why there is none
	Decisions []Decision // same order as the input frames
}

// burst is what the relative checks compare against.
type burst struct {
	size     int
	sharpest float64
	// lowEV and highEV are the middle exposures of the usable frames: equal
	// for an odd count, the two middle values for an even one. A frame is
	// measured against the nearer of them, never against their midpoint,
	// which may match no frame at all (stage light changing mid-burst).
	lowEV, highEV float64
	anyOpen       bool // some usable frame has a face with eyes open
	anyClosed     bool
}

// offEV is how far a frame's exposure is from the burst's middle: zero
// between the two middle values, otherwise the distance to the nearer one.
func (b burst) offEV(ev float64) float64 {
	switch {
	case ev < b.lowEV:
		return ev - b.lowEV
	case ev > b.highEV:
		return ev - b.highEV
	}
	return 0
}

// Decide applies the conference profile to one burst. frames must not be empty.
func Decide(frames []Frame) Result {
	res := Result{Decisions: make([]Decision, len(frames))}

	// 1. Unusable frames are out, whatever the rest of the burst looks like.
	unusable := make([][]string, len(frames))
	var usable []int
	for i, f := range frames {
		if unusable[i] = floor(f); len(unusable[i]) == 0 {
			usable = append(usable, i)
		}
	}

	// 2. The burst the usable frames are judged against.
	b := burst{size: len(usable)}
	var evs []float64
	for _, i := range usable {
		f := frames[i].Features
		b.sharpest = max(b.sharpest, f.Sharpness.Score)
		evs = append(evs, EV(f.Exposure.Mean))
		b.anyOpen = b.anyOpen || f.Eyes.State == events.EyesOpen
		b.anyClosed = b.anyClosed || f.Eyes.State == events.EyesClosed
	}
	b.lowEV, b.highEV = middle(evs)

	// 3. Relative checks; the best frame is the strongest that passes them.
	// If none passes (the whole burst shares a problem), the strongest usable
	// frame is still the best: the moment is kept.
	weaker := make([][]string, len(frames))
	best := -1
	for _, i := range usable {
		weaker[i] = relative(frames[i], b)
		if len(weaker[i]) == 0 && (best < 0 || stronger(frames[i], frames[best], b)) {
			best = i
		}
	}
	if best < 0 {
		for _, i := range usable {
			if best < 0 || stronger(frames[i], frames[best], b) {
				best = i
			}
		}
		if best >= 0 {
			weaker[best] = nil
		}
	}

	for i, f := range frames {
		d := Decision{SHA256: f.SHA256}
		switch {
		case len(unusable[i]) > 0:
			d.Bucket, d.Rating, d.Reasons = events.BucketReject, RatingReject, unusable[i]
		case i == best:
			d.Bucket, d.Rating, d.Reasons = events.BucketBest, RatingBest, []string{bestReason(f, b)}
		case len(weaker[i]) > 0:
			d.Bucket, d.Rating, d.Reasons = events.BucketReject, RatingReject, weaker[i]
		default:
			d.Bucket, d.Rating = events.BucketKeeper, RatingKeeper
			d.Reasons = []string{fmt.Sprintf("as good as the rest of the burst (sharpness %.2f, best %.2f)",
				f.Features.Sharpness.Score, frames[best].Features.Sharpness.Score)}
		}
		res.Decisions[i] = d
	}

	if best >= 0 {
		res.Best = frames[best].SHA256
		res.Reason = res.Decisions[best].Reasons[0]
	} else {
		res.Reason = "no usable frame in this burst"
	}
	return res
}

// floor returns why a frame is unusable on its own; none means usable.
func floor(f Frame) []string {
	var reasons []string
	if s := f.Features.Sharpness.Score; s < UnusableSharpness {
		reasons = append(reasons, fmt.Sprintf("too blurred to use (sharpness %.2f)", s))
	}
	e := f.Features.Exposure
	switch {
	case e.Mean < NearlyBlack:
		reasons = append(reasons, fmt.Sprintf("nearly black (mean brightness %.2f)", e.Mean))
	case e.Mean > BlownOut:
		reasons = append(reasons, fmt.Sprintf("blown out (mean brightness %.2f)", e.Mean))
	case e.HighlightsClipped > BlownOutClipped:
		reasons = append(reasons, fmt.Sprintf("blown out (%.0f %% of highlights clipped)", 100*e.HighlightsClipped))
	}
	return reasons
}

// relative returns why a usable frame is clearly worse than its burst, in
// the order a photographer looks: eyes, focus, light.
func relative(f Frame, b burst) []string {
	if b.size < 2 {
		return nil
	}
	var reasons []string
	// A blink only counts when another frame caught the same people with
	// their eyes open; otherwise the whole burst looks down, and that is the
	// moment.
	if f.Features.Eyes.State == events.EyesClosed && b.anyOpen {
		reasons = append(reasons, "eyes closed (open in another frame)")
	}
	if s := f.Features.Sharpness.Score; s < MinRelativeSharpness*b.sharpest {
		reasons = append(reasons, fmt.Sprintf("blurred next to the rest of the burst (sharpness %.2f, sharpest %.2f)", s, b.sharpest))
	}
	if off := b.offEV(EV(f.Features.Exposure.Mean)); math.Abs(off) > MaxExposureOffEV {
		side := "darker"
		if off > 0 {
			side = "brighter"
		}
		reasons = append(reasons, fmt.Sprintf("%s than the rest of the burst (%s EV)", side, Stops(off)))
	}
	return reasons
}

// stronger ranks usable frames for best. At a conference people come first:
// open eyes beat closed or unseen ones when the burst has a blink in it, a
// visible face beats none, then the sharper frame, then the one nearer the
// burst's middle exposure, then the earlier one, then sha256 so the result never
// depends on input order.
func stronger(a, c Frame, b burst) bool {
	if b.anyOpen && b.anyClosed {
		if ao, co := a.Features.Eyes.State == events.EyesOpen, c.Features.Eyes.State == events.EyesOpen; ao != co {
			return ao
		}
	}
	if af, cf := a.Features.Eyes.Faces > 0, c.Features.Eyes.Faces > 0; af != cf {
		return af
	}
	if x := cmp.Compare(a.Features.Sharpness.Score, c.Features.Sharpness.Score); x != 0 {
		return x > 0
	}
	da := math.Abs(b.offEV(EV(a.Features.Exposure.Mean)))
	dc := math.Abs(b.offEV(EV(c.Features.Exposure.Mean)))
	if x := cmp.Compare(da, dc); x != 0 {
		return x < 0
	}
	if !a.CapturedAt.Equal(c.CapturedAt) {
		return a.CapturedAt.Before(c.CapturedAt)
	}
	return a.SHA256 < c.SHA256
}

func bestReason(f Frame, b burst) string {
	s := f.Features.Sharpness.Score
	if b.size == 1 {
		return fmt.Sprintf("single frame, kept (sharpness %.2f)", s)
	}
	var why []string
	if b.anyOpen && b.anyClosed && f.Features.Eyes.State == events.EyesOpen {
		why = append(why, "eyes open")
	}
	if s >= b.sharpest {
		why = append(why, "sharpest frame")
	} else {
		why = append(why, "strongest frame")
	}
	return fmt.Sprintf("%s (sharpness %.2f)", strings.Join(why, ", "), s)
}

// EV is a frame's exposure in stops from middle grey: the scorer's mean is
// gamma-encoded luminance, so it is linearised and compared with 18 %.
// The console shows the same number.
func EV(mean float64) float64 {
	return math.Log2(math.Max(1e-4, math.Pow(mean, 2.2)) / 0.18)
}

// Stops formats an EV difference in thirds of a stop: "−1 2/3", "+1/3", "±0".
func Stops(ev float64) string {
	thirds := int(math.Round(ev * 3))
	sign := "+"
	switch {
	case thirds < 0:
		sign, thirds = "−", -thirds
	case thirds == 0:
		return "±0"
	}
	whole, rest := thirds/3, thirds%3
	switch {
	case rest == 0:
		return fmt.Sprintf("%s%d", sign, whole)
	case whole == 0:
		return fmt.Sprintf("%s%d/3", sign, rest)
	}
	return fmt.Sprintf("%s%d %d/3", sign, whole, rest)
}

// middle returns the middle value of xs twice for an odd count, and the two
// middle values for an even one.
func middle(xs []float64) (low, high float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2], s[n/2]
	}
	return s[n/2-1], s[n/2]
}
