package policy

import (
	"slices"
	"testing"
	"time"

	"github.com/geilername/keeper/internal/events"
)

var t0 = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

// frame builds a test frame: sharpness score, exposure score, eye state.
func frame(sha string, sharp, expo float64, eyes string) Frame {
	return Frame{
		SHA256:     sha,
		CapturedAt: t0,
		Features: events.Features{
			Sharpness: events.Sharpness{Score: sharp},
			Exposure:  events.Exposure{Score: expo, Mean: 0.5},
			Eyes:      events.Eyes{State: eyes},
		},
	}
}

func dark(f Frame) Frame   { f.Features.Exposure.Mean = 0.08; return f }
func bright(f Frame) Frame { f.Features.Exposure.Mean = 0.93; return f }

func TestDecide(t *testing.T) {
	type want struct {
		bucket  string
		reasons []string
	}
	const open, closed, noFace = events.EyesOpen, events.EyesClosed, events.EyesNoFace

	tests := []struct {
		name    string
		frames  []Frame
		want    []want
		best    string
		bestWhy string
	}{
		{
			name:   "single sharp frame is best",
			frames: []Frame{frame("a", 0.8, 0.7, open)},
			want:   []want{{events.BucketBest, []string{"single frame, sharp and well exposed with eyes open (sharpness 0.80)"}}},
			best:   "a", bestWhy: "single frame, sharp and well exposed with eyes open (sharpness 0.80)",
		},
		{
			name:   "single frame without a face",
			frames: []Frame{frame("a", 0.8, 0.7, noFace)},
			want:   []want{{events.BucketBest, []string{"single frame, sharp and well exposed (sharpness 0.80)"}}},
			best:   "a", bestWhy: "single frame, sharp and well exposed (sharpness 0.80)",
		},
		{
			name:   "single blurred frame is a reject",
			frames: []Frame{frame("a", 0.3, 0.7, open)},
			want:   []want{{events.BucketReject, []string{"blurred (sharpness 0.30)"}}},
			best:   "", bestWhy: "no frame passed the checks",
		},
		{
			name:   "single frame with eyes closed is a reject",
			frames: []Frame{frame("a", 0.9, 0.7, closed)},
			want:   []want{{events.BucketReject, []string{"eyes closed"}}},
			best:   "", bestWhy: "no frame passed the checks",
		},
		{
			name:   "underexposed",
			frames: []Frame{dark(frame("a", 0.9, 0.2, open))},
			want:   []want{{events.BucketReject, []string{"underexposed (exposure 0.20, mean brightness 0.08)"}}},
			best:   "", bestWhy: "no frame passed the checks",
		},
		{
			name:   "overexposed",
			frames: []Frame{bright(frame("a", 0.9, 0.1, open))},
			want:   []want{{events.BucketReject, []string{"overexposed (exposure 0.10, mean brightness 0.93)"}}},
			best:   "", bestWhy: "no frame passed the checks",
		},
		{
			name:   "every failure is named, eyes first",
			frames: []Frame{dark(frame("a", 0.2, 0.1, closed))},
			want: []want{{events.BucketReject, []string{
				"eyes closed", "blurred (sharpness 0.20)", "underexposed (exposure 0.10, mean brightness 0.08)",
			}}},
			best: "", bestWhy: "no frame passed the checks",
		},
		{
			name: "demo burst: one best, two keepers, three rejects",
			frames: []Frame{
				frame("a", 0.74, 0.7, open),   // keeper
				frame("b", 0.88, 0.6, closed), // sharpest, but blinking
				frame("c", 0.83, 0.7, open),   // best
				frame("d", 0.21, 0.7, open),   // motion blur
				frame("e", 0.79, 0.7, open),   // keeper
				dark(frame("f", 0.80, 0.2, open)),
			},
			want: []want{
				{events.BucketKeeper, []string{"sharp and well exposed, second to the best frame (sharpness 0.74 vs 0.83)"}},
				{events.BucketReject, []string{"eyes closed"}},
				{events.BucketBest, []string{"sharpest frame with eyes open (sharpness 0.83)"}},
				{events.BucketReject, []string{"motion blur (sharpness 0.21, sharpest in burst 0.88)"}},
				{events.BucketKeeper, []string{"sharp and well exposed, second to the best frame (sharpness 0.79 vs 0.83)"}},
				{events.BucketReject, []string{"underexposed (exposure 0.20, mean brightness 0.08)"}},
			},
			best: "c", bestWhy: "sharpest frame with eyes open (sharpness 0.83)",
		},
		{
			name: "sharp in absolute terms but soft next to the sharpest frame",
			frames: []Frame{
				frame("a", 0.95, 0.7, open),
				frame("b", 0.55, 0.7, open), // above MinSharpness, below 0.6 × 0.95
			},
			want: []want{
				{events.BucketBest, []string{"sharpest frame with eyes open (sharpness 0.95)"}},
				{events.BucketReject, []string{"motion blur (sharpness 0.55, sharpest in burst 0.95)"}},
			},
			best: "a", bestWhy: "sharpest frame with eyes open (sharpness 0.95)",
		},
		{
			name: "whole burst blinked: no best",
			frames: []Frame{
				frame("a", 0.9, 0.7, closed),
				frame("b", 0.8, 0.7, closed),
			},
			want: []want{
				{events.BucketReject, []string{"eyes closed"}},
				{events.BucketReject, []string{"eyes closed"}},
			},
			best: "", bestWhy: "no frame passed the checks",
		},
		{
			name: "equal sharpness: better exposure wins",
			frames: []Frame{
				frame("a", 0.8, 0.5, open),
				frame("b", 0.8, 0.9, open),
			},
			want: []want{
				{events.BucketKeeper, []string{"sharp and well exposed, second to the best frame (sharpness 0.80 vs 0.80)"}},
				{events.BucketBest, []string{"sharpest frame with eyes open (sharpness 0.80)"}},
			},
			best: "b", bestWhy: "sharpest frame with eyes open (sharpness 0.80)",
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

// The outcome must not depend on the order frames arrive in.
func TestDecideIgnoresInputOrder(t *testing.T) {
	a := frame("a", 0.8, 0.7, events.EyesOpen)
	b := frame("b", 0.8, 0.7, events.EyesOpen) // identical scores: sha breaks the tie
	if got1, got2 := Decide([]Frame{a, b}).Best, Decide([]Frame{b, a}).Best; got1 != "a" || got2 != "a" {
		t.Fatalf("best = %q / %q, want a / a", got1, got2)
	}
}
