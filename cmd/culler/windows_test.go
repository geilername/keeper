package main

import (
	"testing"
	"time"

	"github.com/geilername/keeper/internal/events"
	"github.com/geilername/keeper/internal/policy"
)

var (
	capture0 = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	wall0    = time.Date(2026, 11, 9, 13, 0, 0, 0, time.UTC)
)

var testCfg = windowConfig{Gap: 2 * time.Second, MaxHamming: 12, Idle: 20 * time.Second, MaxWait: 90 * time.Second}

const (
	hashA     = "f0f0f0f0f0f0f0f0"
	hashNearA = "f0f0f0f0f0f0f0ff" // 4 bits off
	hashFarA  = "0f0f0f0f0f0f0f0f" // 64 bits off
)

type fakeMsg struct{ acks, progress int }

func (m *fakeMsg) Ack() error        { m.acks++; return nil }
func (m *fakeMsg) InProgress() error { m.progress++; return nil }

func mem(sha string, captured time.Duration, dhash string) *member {
	return &member{
		frame: policy.Frame{SHA256: sha, CapturedAt: capture0.Add(captured), Features: events.Features{DHash: dhash}},
		msg:   &fakeMsg{},
	}
}

func TestGrouping(t *testing.T) {
	tests := []struct {
		name    string
		second  *member
		want    addResult
		windows int
	}{
		{"close in time, looks alike: same burst", mem("b", 1500*time.Millisecond, hashNearA), joined, 1},
		{"gap too long: new burst", mem("b", 2500*time.Millisecond, hashA), opened, 2},
		{"looks different: new burst", mem("b", 500*time.Millisecond, hashFarA), opened, 2},
		{"captured earlier than the first: same burst", mem("b", -time.Second, hashA), joined, 1},
		{"unreadable dhash: new burst", mem("b", 0, "zz"), opened, 2},
		{"same photo redelivered: not counted twice", mem("a", 0, hashA), redelivered, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWindows(testCfg)
			w.add(mem("a", 0, hashA), wall0)
			if got := w.add(tt.second, wall0.Add(time.Second)); got != tt.want {
				t.Errorf("add = %v, want %v", got, tt.want)
			}
			if len(w.open) != tt.windows {
				t.Errorf("%d windows open, want %d", len(w.open), tt.windows)
			}
			wantHeld := 2
			if tt.want == redelivered {
				wantHeld = 1
			}
			if len(w.bySHA) != wantHeld {
				t.Errorf("%d photos held, want %d", len(w.bySHA), wantHeld)
			}
		})
	}
}

func TestRedeliveryKeepsNewestMessage(t *testing.T) {
	w := newWindows(testCfg)
	first, again := mem("a", 0, hashA), mem("a", 0, hashA)
	w.add(first, wall0)
	w.add(again, wall0.Add(31*time.Second))
	if first.msg != again.msg {
		t.Error("held member must ack the newest delivery")
	}
	if got := w.due(wall0.Add(21 * time.Second)); len(got) != 1 {
		t.Error("a redelivery must not reset the idle timer")
	}
}

// A long sequence of frames, each within the gap of the previous one, is one
// burst even though first and last are far apart.
func TestChainedFramesStayOneBurst(t *testing.T) {
	w := newWindows(testCfg)
	for i, sha := range []string{"a", "b", "c", "d", "e"} {
		w.add(mem(sha, time.Duration(i)*1500*time.Millisecond, hashA), wall0)
	}
	if len(w.open) != 1 || len(w.open[0].members) != 5 {
		t.Fatalf("want 1 window with 5 members, got %d windows", len(w.open))
	}
}

func TestClosing(t *testing.T) {
	w := newWindows(testCfg)
	w.add(mem("a", 0, hashA), wall0)

	if got := w.due(wall0.Add(19 * time.Second)); len(got) != 0 {
		t.Fatalf("closed too early: %v", got)
	}
	if got := w.due(wall0.Add(20 * time.Second)); len(got) != 1 || got[0].reason != "idle" {
		t.Fatalf("want idle close at 20s, got %v", got)
	}

	// A frame every 10 s keeps the window busy; max_wait still closes it.
	for i := 1; i <= 8; i++ {
		w.add(mem(string(rune('a'+i)), time.Duration(i)*time.Second, hashA), wall0.Add(time.Duration(i)*10*time.Second))
	}
	if got := w.due(wall0.Add(89 * time.Second)); len(got) != 0 {
		t.Fatalf("closed too early: %v", got)
	}
	if got := w.due(wall0.Add(90 * time.Second)); len(got) != 1 || got[0].reason != "max_wait" {
		t.Fatalf("want max_wait close at 90s, got %v", got)
	}
}

func TestDecidedPhotoIsRemembered(t *testing.T) {
	w := newWindows(testCfg)
	w.add(mem("a", 0, hashA), wall0)
	w.remove(w.open[0], wall0)
	if got := w.add(mem("a", 0, hashA), wall0.Add(time.Minute)); got != alreadyDone {
		t.Fatalf("add after decide = %v, want alreadyDone", got)
	}
	w.forget(wall0.Add(forgetAfter+time.Second), forgetAfter)
	if got := w.add(mem("a", 0, hashA), wall0.Add(forgetAfter+time.Second)); got != opened {
		t.Fatalf("after forget: add = %v, want opened", got)
	}
}

func TestHamming(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want int
	}{
		{hashA, hashA, 0},
		{hashA, hashNearA, 4},
		{hashA, hashFarA, 64},
		{hashA, "", 64},
	} {
		if got := hamming(tt.a, tt.b); got != tt.want {
			t.Errorf("hamming(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}
