package main

import (
	"math/bits"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/geilername/keeper/internal/policy"
)

// Burst windows (ADR-4). This file is plain data: no NATS, no OTel export,
// no locking. The culler guards it with one mutex.
//
// Two clocks are involved:
//   - capture time (from EXIF) decides which burst a photo belongs to;
//   - arrival time (wall clock here) decides when a window closes.

type windowConfig struct {
	Gap        time.Duration // capture-time gap that starts a new burst
	MaxHamming int           // dhash distance above which a frame starts a new burst
	Idle       time.Duration // close when no member joined for this long
	MaxWait    time.Duration // close at the latest this long after the first member
}

// heldMsg is the part of a JetStream message the windows need.
type heldMsg interface {
	Ack() error
	InProgress() error
}

// member is one photo held in an open window, message still unacked.
type member struct {
	frame   policy.Frame
	msg     heldMsg
	parent  trace.SpanContext // CONSUMER span of the held delivery: parent of the decision span
	arrived time.Time
}

type window struct {
	members []*member
	opened  time.Time // arrival of the first member
	touched time.Time // arrival of the latest member
}

type windows struct {
	cfg   windowConfig
	open  []*window
	bySHA map[string]*member // photos in open windows

	// decided remembers recently decided photos, so a redelivery after a
	// lost ack is acked instead of starting a burst of its own.
	decided map[string]time.Time
}

func newWindows(cfg windowConfig) *windows {
	return &windows{cfg: cfg, bySHA: map[string]*member{}, decided: map[string]time.Time{}}
}

type addResult int

const (
	joined      addResult = iota // added to an open window
	opened                       // started a new window
	redelivered                  // already in an open window: message swapped, not counted twice
	alreadyDone                  // decided recently: ack and forget
)

// add puts m into the window it belongs to, or opens a new one.
func (w *windows) add(m *member, now time.Time) addResult {
	sha := m.frame.SHA256
	if _, ok := w.decided[sha]; ok {
		return alreadyDone
	}
	if old, ok := w.bySHA[sha]; ok {
		// JetStream redelivered a photo we hold (AckWait ran out, or the
		// culler restarted). Keep the newest delivery: that is the one to ack.
		old.msg, old.parent = m.msg, m.parent
		return redelivered
	}
	m.arrived = now
	w.bySHA[sha] = m

	if win := w.match(m.frame); win != nil {
		win.members = append(win.members, m)
		win.touched = now
		return joined
	}
	w.open = append(w.open, &window{members: []*member{m}, opened: now, touched: now})
	return opened
}

// match finds the open window with a member captured within Gap of f that
// also looks alike (dhash). The closest capture time wins.
func (w *windows) match(f policy.Frame) *window {
	var best *window
	bestGap := w.cfg.Gap + 1
	for _, win := range w.open {
		for _, m := range win.members {
			gap := f.CapturedAt.Sub(m.frame.CapturedAt).Abs()
			if gap <= w.cfg.Gap && gap < bestGap &&
				hamming(f.Features.DHash, m.frame.Features.DHash) <= w.cfg.MaxHamming {
				best, bestGap = win, gap
			}
		}
	}
	return best
}

type closing struct {
	win    *window
	reason string // "idle" | "max_wait"
}

// due returns the windows that should close now, oldest first.
func (w *windows) due(now time.Time) []closing {
	var out []closing
	for _, win := range w.open {
		switch {
		case now.Sub(win.opened) >= w.cfg.MaxWait:
			out = append(out, closing{win, "max_wait"})
		case now.Sub(win.touched) >= w.cfg.Idle:
			out = append(out, closing{win, "idle"})
		}
	}
	return out
}

// remove drops a decided window and remembers its photos for a while.
func (w *windows) remove(win *window, now time.Time) {
	for i, o := range w.open {
		if o == win {
			w.open = append(w.open[:i], w.open[i+1:]...)
			break
		}
	}
	for _, m := range win.members {
		delete(w.bySHA, m.frame.SHA256)
		w.decided[m.frame.SHA256] = now
	}
}

// forget drops decided photos older than keep.
func (w *windows) forget(now time.Time, keep time.Duration) {
	for sha, at := range w.decided {
		if now.Sub(at) > keep {
			delete(w.decided, sha)
		}
	}
}

// hamming counts differing bits between two 64-bit dhash hex strings. An
// unreadable hash counts as completely different.
func hamming(a, b string) int {
	x, errA := strconv.ParseUint(a, 16, 64)
	y, errB := strconv.ParseUint(b, 16, 64)
	if errA != nil || errB != nil {
		return 64
	}
	return bits.OnesCount64(x ^ y)
}
