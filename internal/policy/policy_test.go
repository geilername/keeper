package policy

import (
	"slices"
	"testing"
	"time"

	"github.com/geilername/keeper/internal/events"
)

var t0 = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

const open, closed, noFace = events.EyesOpen, events.EyesClosed, events.EyesNoFace

// frame builds a test frame: sharpness score, mean brightness, eye state.
// Open or closed eyes imply one detected face.
func frame(sha string, sharp, mean float64, eyes string) Frame {
	faces := 0
	if eyes != noFace {
		faces = 1
	}
	return Frame{
		SHA256:     sha,
		CapturedAt: t0,
		Features: events.Features{
			Sharpness: events.Sharpness{Score: sharp},
			Exposure:  events.Exposure{Mean: mean},
			Eyes:      events.Eyes{State: eyes, Faces: faces},
		},
	}
}

func TestDecide(t *testing.T) {
	type want struct {
		bucket  string
		reasons []string
	}
	tests := []struct {
		name    string
		frames  []Frame
		want    []want
		best    string
		bestWhy string
	}{
		{
			name:   "single usable frame is kept",
			frames: []Frame{frame("a", 0.59, 0.4, open)},
			want:   []want{{events.BucketBest, []string{"single frame, kept (sharpness 0.59)"}}},
			best:   "a", bestWhy: "single frame, kept (sharpness 0.59)",
		},
		{
			name:   "single frame too blurred to use",
			frames: []Frame{frame("a", 0.20, 0.4, open)},
			want:   []want{{events.BucketReject, []string{"too blurred to use (sharpness 0.20)"}}},
			best:   "", bestWhy: "no usable frame in this burst",
		},
		{
			name:   "nearly black and blown out are unusable",
			frames: []Frame{frame("a", 0.6, 0.02, noFace), frame("b", 0.6, 0.97, noFace)},
			want: []want{
				{events.BucketReject, []string{"nearly black (mean brightness 0.02)"}},
				{events.BucketReject, []string{"blown out (mean brightness 0.97)"}},
			},
			best: "", bestWhy: "no usable frame in this burst",
		},
		{
			// Real case: a dim hall, every frame equally dark. The venue is
			// dark; nothing is underexposed relative to the scene.
			name: "dark venue: the whole burst shares the darkness",
			frames: []Frame{
				frame("a", 0.59, 0.14, open),
				frame("b", 0.59, 0.14, open),
				frame("c", 0.52, 0.14, open),
			},
			want: []want{
				{events.BucketBest, []string{"sharpest frame (sharpness 0.59)"}},
				{events.BucketKeeper, []string{"as good as the rest of the burst (sharpness 0.59, best 0.59)"}},
				{events.BucketKeeper, []string{"as good as the rest of the burst (sharpness 0.52, best 0.59)"}},
			},
			best: "a", bestWhy: "sharpest frame (sharpness 0.59)",
		},
		{
			// Real case: two people looking down at a phone. Haar reads
			// downcast eyes as closed in every frame: that is the moment.
			name: "everyone looks down: closed eyes in every frame are not a blink",
			frames: []Frame{
				frame("a", 0.69, 0.3, closed),
				frame("b", 0.70, 0.3, closed),
			},
			want: []want{
				{events.BucketKeeper, []string{"as good as the rest of the burst (sharpness 0.69, best 0.70)"}},
				{events.BucketBest, []string{"sharpest frame (sharpness 0.70)"}},
			},
			best: "b", bestWhy: "sharpest frame (sharpness 0.70)",
		},
		{
			name: "a real blink: closed eyes lose to the frame that caught them open",
			frames: []Frame{
				frame("a", 0.70, 0.3, closed),
				frame("b", 0.60, 0.3, open),
			},
			want: []want{
				{events.BucketReject, []string{"eyes closed (open in another frame)"}},
				{events.BucketBest, []string{"eyes open, strongest frame (sharpness 0.60)"}},
			},
			best: "b", bestWhy: "eyes open, strongest frame (sharpness 0.60)",
		},
		{
			name: "blurred next to its siblings",
			frames: []Frame{
				frame("a", 0.59, 0.4, open),
				frame("b", 0.30, 0.4, open),
			},
			want: []want{
				{events.BucketBest, []string{"sharpest frame (sharpness 0.59)"}},
				{events.BucketReject, []string{"blurred next to the rest of the burst (sharpness 0.30, sharpest 0.59)"}},
			},
			best: "a", bestWhy: "sharpest frame (sharpness 0.59)",
		},
		{
			name: "a stop and more off the burst's exposure",
			frames: []Frame{
				frame("a", 0.60, 0.4, noFace),
				frame("b", 0.60, 0.4, noFace),
				frame("c", 0.65, 0.2, noFace),
			},
			want: []want{
				{events.BucketBest, []string{"strongest frame (sharpness 0.60)"}},
				{events.BucketKeeper, []string{"as good as the rest of the burst (sharpness 0.60, best 0.60)"}},
				{events.BucketReject, []string{"darker than the rest of the burst (−2 1/3 EV)"}},
			},
			best: "a", bestWhy: "strongest frame (sharpness 0.60)",
		},
		{
			name: "a visible face beats a sharper frame without one",
			frames: []Frame{
				frame("a", 0.70, 0.4, noFace),
				frame("b", 0.60, 0.4, open),
			},
			want: []want{
				{events.BucketKeeper, []string{"as good as the rest of the burst (sharpness 0.70, best 0.60)"}},
				{events.BucketBest, []string{"strongest frame (sharpness 0.60)"}},
			},
			best: "b", bestWhy: "strongest frame (sharpness 0.60)",
		},
		{
			name: "unusable frames do not drag the burst: judged among the usable ones",
			frames: []Frame{
				frame("a", 0.10, 0.4, open),
				frame("b", 0.40, 0.4, open),
			},
			want: []want{
				{events.BucketReject, []string{"too blurred to use (sharpness 0.10)"}},
				{events.BucketBest, []string{"single frame, kept (sharpness 0.40)"}},
			},
			best: "b", bestWhy: "single frame, kept (sharpness 0.40)",
		},
		{
			name: "every frame worse in some way: the strongest is still kept",
			frames: []Frame{
				frame("a", 0.60, 0.4, closed),
				frame("b", 0.30, 0.4, open),
			},
			want: []want{
				{events.BucketReject, []string{"eyes closed (open in another frame)"}},
				{events.BucketBest, []string{"eyes open, strongest frame (sharpness 0.30)"}},
			},
			best: "b", bestWhy: "eyes open, strongest frame (sharpness 0.30)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := Decide(tt.frames)
			if res.Best != tt.best || res.Reason != tt.bestWhy {
				t.Errorf("best = %q (%q), want %q (%q)", res.Best, res.Reason, tt.best, tt.bestWhy)
			}
			if len(res.Decisions) != len(tt.want) {
				t.Fatalf("%d decisions, want %d", len(res.Decisions), len(tt.want))
			}
			for i, d := range res.Decisions {
				w := tt.want[i]
				if d.SHA256 != tt.frames[i].SHA256 || d.Bucket != w.bucket || !slices.Equal(d.Reasons, w.reasons) {
					t.Errorf("frame %s: got %s %q, want %s %q", tt.frames[i].SHA256, d.Bucket, d.Reasons, w.bucket, w.reasons)
				}
				wantRating := map[string]int{events.BucketBest: 5, events.BucketKeeper: 3, events.BucketReject: 1}[w.bucket]
				if d.Rating != wantRating {
					t.Errorf("frame %s: rating %d, want %d", d.SHA256, d.Rating, wantRating)
				}
			}
		})
	}
}

// Every burst with a usable frame keeps exactly one best.
func TestExactlyOneBestWhenAnythingIsUsable(t *testing.T) {
	for _, frames := range [][]Frame{
		{frame("a", 0.3, 0.1, closed), frame("b", 0.3, 0.1, closed), frame("c", 0.26, 0.9, noFace)},
		{frame("a", 0.9, 0.5, open), frame("b", 0.9, 0.5, open)},
	} {
		n := 0
		for _, d := range Decide(frames).Decisions {
			if d.Bucket == events.BucketBest {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%d best frames, want exactly 1", n)
		}
	}
}

// The outcome must not depend on the order frames arrive in.
func TestDecideIgnoresInputOrder(t *testing.T) {
	a := frame("a", 0.8, 0.4, open)
	b := frame("b", 0.8, 0.4, open) // identical scores: sha breaks the tie
	if got1, got2 := Decide([]Frame{a, b}).Best, Decide([]Frame{b, a}).Best; got1 != "a" || got2 != "a" {
		t.Fatalf("best = %q / %q, want a / a", got1, got2)
	}
}

func TestStops(t *testing.T) {
	for ev, want := range map[float64]string{0: "±0", 0.34: "+1/3", -0.66: "−2/3", 1: "+1", -1.66: "−1 2/3", 2.34: "+2 1/3"} {
		if got := Stops(ev); got != want {
			t.Errorf("Stops(%v) = %q, want %q", ev, got, want)
		}
	}
}
