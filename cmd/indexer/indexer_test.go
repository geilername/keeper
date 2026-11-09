package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/geilername/keeper/internal/events"
	"github.com/geilername/keeper/internal/photostate"
	"github.com/geilername/keeper/internal/search"
)

const testSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

var t0 = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

// fakeES stores documents as JSON, like Elasticsearch's _source, so the
// indexer sees exactly what a JSON round trip gives it.
type fakeES struct {
	docs   map[string][]byte
	seq    map[string]int64
	puts   int
	before func() // runs at the start of every Put: simulates a concurrent writer
	putErr error
}

func newFake() *fakeES { return &fakeES{docs: map[string][]byte{}, seq: map[string]int64{}} }

func (f *fakeES) Get(_ context.Context, _, id string, v any) (search.Version, bool, error) {
	d, ok := f.docs[id]
	if !ok {
		return search.Version{}, false, nil
	}
	return search.Version{SeqNo: f.seq[id], PrimaryTerm: 1}, true, json.Unmarshal(d, v)
}

func (f *fakeES) Put(_ context.Context, _, id string, v any, ver *search.Version) error {
	if f.before != nil {
		f.before()
	}
	if f.putErr != nil {
		return f.putErr
	}
	_, exists := f.docs[id]
	if (ver == nil && exists) || (ver != nil && (!exists || ver.SeqNo != f.seq[id])) {
		return search.ErrConflict
	}
	d, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f.docs[id] = d
	f.seq[id]++
	f.puts++
	return nil
}

func (f *fakeES) photo(t *testing.T) photostate.Photo {
	t.Helper()
	var p photostate.Photo
	if err := json.Unmarshal(f.docs[testSHA], &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func newIndexer(es docStore) *indexer {
	return &indexer{es: es, now: func() time.Time { return t0.Add(time.Hour) }}
}

var (
	ingested = &events.PhotoIngested{SHA256: testSHA, FileName: "DSC0001.ARW", CapturedAt: t0}
	scored   = &events.PhotoScored{SHA256: testSHA, CapturedAt: t0, Features: events.Features{
		Sharpness: events.Sharpness{Variance: 812.25, Score: 0.11},
		Eyes:      events.Eyes{State: events.EyesClosed, Confidence: 0.93, Faces: 1},
		DHash:     "00ff00ff00ff00ff"}}
	decided = &events.PhotoDecided{SHA256: testSHA, BurstID: "b1", Bucket: events.BucketReject,
		Rating: 1, Reasons: []string{"eyes closed", "motion blur (sharpness 0.11, sharpest in burst 0.59)"}}
	published = &events.PhotoPublished{SHA256: testSHA, LightroomAssetID: "a1", Attempt: 1}
)

func TestIndexerFoldsEventsIntoOneDocument(t *testing.T) {
	es := newFake()
	ix := newIndexer(es)
	ctx := context.Background()
	for i, ev := range []any{ingested, scored, decided, published} {
		if err := ix.apply(ctx, testSHA, ev, t0.Add(time.Duration(i+1)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	p := es.photo(t)
	if p.Stage != photostate.StagePublished || p.FileName != "DSC0001.ARW" || p.Rating != 1 ||
		p.Features == nil || p.Features.Eyes.State != events.EyesClosed || len(p.Reasons) != 2 ||
		p.LightroomAssetID != "a1" {
		t.Errorf("document = %+v", p)
	}
	if es.puts != 4 {
		t.Errorf("puts = %d, want 4", es.puts)
	}
}

func TestRedeliveryWritesNothing(t *testing.T) {
	es := newFake()
	ix := newIndexer(es)
	ctx := context.Background()
	for _, ev := range []any{ingested, scored, decided, published} {
		if err := ix.apply(ctx, testSHA, ev, t0); err != nil {
			t.Fatal(err)
		}
	}
	puts := es.puts
	for _, ev := range []any{ingested, scored, decided, published} {
		if err := ix.apply(ctx, testSHA, ev, t0.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if es.puts != puts {
		t.Errorf("redelivered events wrote %d times, want 0 (the JSON round trip must compare equal)",
			es.puts-puts)
	}
}

func TestLateEventDoesNotMoveStageBack(t *testing.T) {
	es := newFake()
	ix := newIndexer(es)
	ctx := context.Background()
	for _, ev := range []any{decided, published, scored} {
		if err := ix.apply(ctx, testSHA, ev, t0); err != nil {
			t.Fatal(err)
		}
	}
	if p := es.photo(t); p.Stage != photostate.StagePublished || p.Features == nil {
		t.Errorf("stage=%q features=%v", p.Stage, p.Features)
	}
}

func TestConflictRetriesFromNewerDocument(t *testing.T) {
	es := newFake()
	ix := newIndexer(es)
	ctx := context.Background()
	if err := ix.apply(ctx, testSHA, ingested, t0); err != nil {
		t.Fatal(err)
	}
	// Another writer folds "decided" in just before our first write.
	other := newIndexer(es)
	es.before = func() {
		es.before = nil
		if err := other.apply(ctx, testSHA, decided, t0.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if err := ix.apply(ctx, testSHA, scored, t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	p := es.photo(t)
	if p.Stage != photostate.StageDecided || p.Features == nil || p.Bucket != events.BucketReject {
		t.Errorf("lost an update: stage=%q features=%v bucket=%q", p.Stage, p.Features, p.Bucket)
	}
}

func TestEndlessConflictsGoBackToJetStream(t *testing.T) {
	es := newFake()
	es.putErr = search.ErrConflict
	err := newIndexer(es).apply(context.Background(), testSHA, ingested, t0)
	if err == nil || isPermanent(err) {
		t.Errorf("err = %v, want a retryable error", err)
	}
}

func TestRejectedDocumentIsPermanent(t *testing.T) {
	tests := []struct {
		status    int
		permanent bool
	}{
		{http.StatusBadRequest, true},
		{http.StatusTooManyRequests, false},
		{http.StatusServiceUnavailable, false},
	}
	for _, tt := range tests {
		es := newFake()
		es.putErr = &search.Error{Op: "index", Status: tt.status, Type: "x"}
		err := newIndexer(es).apply(context.Background(), testSHA, ingested, t0)
		if err == nil || isPermanent(err) != tt.permanent {
			t.Errorf("status %d: err = %v, permanent = %v, want %v", tt.status, err, isPermanent(err), tt.permanent)
		}
	}
}

// isPermanent: natsx.Permanent wraps the error, so a permanent failure is a
// *search.Error underneath but not on top.
func isPermanent(err error) bool {
	var e *search.Error
	return errors.As(err, &e) && err != error(e)
}
