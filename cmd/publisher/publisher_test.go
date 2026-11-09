package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/geilername/keeper/internal/events"
	"github.com/geilername/keeper/internal/lightroom"
	"github.com/geilername/keeper/internal/lightroom/lightroomtest"
)

type emitted struct {
	subject, msgID string
	v              any
}

type harness struct {
	p      *publisher
	lr     *lightroomtest.Server
	spans  *tracetest.SpanRecorder
	reader *sdkmetric.ManualReader
	now    time.Time

	mu     sync.Mutex
	events []emitted
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	t.Setenv("SPOOL_DIR", t.TempDir())
	h := &harness{
		lr:     lightroomtest.NewServer(lightroom.DefaultCatalogID),
		spans:  tracetest.NewSpanRecorder(),
		reader: sdkmetric.NewManualReader(),
		now:    time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC),
	}
	t.Cleanup(h.lr.Close)
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(h.spans))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(h.reader))
	emit := func(_ context.Context, subject, msgID string, v any) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.events = append(h.events, emitted{subject, msgID, v})
		return nil
	}
	lr := lightroom.NewHTTP(h.lr.URL, lightroom.DefaultCatalogID, time.Second)
	p, err := newPublisher(lr, emit, consumerConfig, tp.Tracer("test"), mp.Meter("test"))
	if err != nil {
		t.Fatal(err)
	}
	p.now = func() time.Time { return h.now }
	p.ratedDir = filepath.Join(t.TempDir(), "rated")
	h.p = p
	return h
}

// photo writes content to the spool and returns its decision event.
func (h *harness) photo(t *testing.T, content string) events.PhotoDecided {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	sha := hex.EncodeToString(sum[:])
	path := filepath.Join(os.Getenv("SPOOL_DIR"), "photos", sha+".jpg")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return events.PhotoDecided{V: 1, SHA256: sha, Bucket: events.BucketBest, Rating: 5}
}

func (h *harness) last(t *testing.T) emitted {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.events) == 0 {
		t.Fatal("no events emitted")
	}
	return h.events[len(h.events)-1]
}

func (h *harness) counter(t *testing.T, name, key, value string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if sum, ok := m.Data.(metricdata.Sum[int64]); ok && m.Name == name {
				for _, dp := range sum.DataPoints {
					if v, _ := dp.Attributes.Value(attribute.Key(key)); v.AsString() == value {
						return dp.Value
					}
				}
			}
		}
	}
	return 0
}

func TestPublishIsIdempotent(t *testing.T) {
	h := newHarness(t)
	ev := h.photo(t, "jpeg one")
	id := lightroom.AssetID(ev.SHA256)

	// Redelivery after a crash: the same message runs twice.
	for delivered := 1; delivered <= 2; delivered++ {
		permanent, err := h.p.attempt(context.Background(), delivered, ev)
		if err != nil || permanent {
			t.Fatalf("delivery %d: permanent=%v err=%v", delivered, permanent, err)
		}
		got := h.last(t)
		pub, ok := got.v.(events.PhotoPublished)
		if !ok || got.subject != events.SubjectPublished || pub.Attempt != delivered || pub.LightroomAssetID != id {
			t.Fatalf("delivery %d emitted %+v", delivered, got)
		}
		// Same dedup ID both times: JetStream drops the second event.
		if got.msgID != "photo.published:"+ev.SHA256 {
			t.Fatalf("msgID = %q", got.msgID)
		}
	}

	a, ok := h.lr.Get(id)
	if !ok || h.lr.Len() != 1 || a.MasterSHA256 != ev.SHA256 || a.Rating != 5 {
		t.Fatalf("lightroom has %d assets, %+v", h.lr.Len(), a)
	}
	if n := h.lr.Calls("create_asset", http.StatusCreated); n != 1 {
		t.Fatalf("assets created %d times", n)
	}
	// The replay saw the master in the create response and skipped the upload.
	if up, again := h.lr.Calls("upload_master", 201), h.lr.Calls("upload_master", 200); up != 1 || again != 0 {
		t.Fatalf("uploads: %d new, %d repeated", up, again)
	}
	if n := h.counter(t, "keeper.photos", "outcome", "published"); n != 2 {
		t.Fatalf("keeper.photos{published} = %d", n)
	}
}

func TestResumeAfterCrashBeforeUpload(t *testing.T) {
	h := newHarness(t)
	ev := h.photo(t, "jpeg two")
	id := lightroom.AssetID(ev.SHA256)
	h.lr.Put(id, lightroomtest.Asset{SHA256: ev.SHA256}) // created, then crashed

	if _, err := h.p.attempt(context.Background(), 2, ev); err != nil {
		t.Fatal(err)
	}
	if a, _ := h.lr.Get(id); h.lr.Len() != 1 || a.MasterSHA256 != ev.SHA256 || a.Rating != 5 {
		t.Fatalf("lightroom has %d assets, %+v", h.lr.Len(), a)
	}
}

func TestRetryOn503ThenHeal(t *testing.T) {
	h := newHarness(t)
	ev := h.photo(t, "jpeg three")
	h.lr.Fail(http.StatusServiceUnavailable)

	for delivered, wait := range map[int]time.Duration{1: 2 * time.Second, 2: 5 * time.Second, 5: 30 * time.Second} {
		permanent, err := h.p.attempt(context.Background(), delivered, ev)
		if err == nil || permanent {
			t.Fatalf("delivery %d: permanent=%v err=%v, want retry", delivered, permanent, err)
		}
		got := h.last(t)
		f, ok := got.v.(events.PhotoFailed)
		if !ok || f.ErrorClass != lightroom.ClassServer || f.Attempt != delivered {
			t.Fatalf("delivery %d emitted %+v", delivered, got)
		}
		if f.RetryAt == nil || !f.RetryAt.Equal(h.now.Add(wait)) {
			t.Fatalf("delivery %d retry_at = %v, want now+%v", delivered, f.RetryAt, wait)
		}
		if want := "photo.failed:" + ev.SHA256 + ":" + strconv.Itoa(delivered); got.msgID != want {
			t.Fatalf("msgID = %q, want %q", got.msgID, want)
		}
	}
	if n := h.counter(t, "keeper.publish.attempts", "result", "retry"); n != 3 {
		t.Fatalf("attempts{retry} = %d", n)
	}

	// The failed attempts are red publish spans with error.type.
	var red int
	for _, s := range h.spans.Ended() {
		if s.Name() == "publish" && s.Status().Code == codes.Error {
			for _, kv := range s.Attributes() {
				if kv.Key == "error.type" && kv.Value.AsString() == lightroom.ClassServer {
					red++
				}
			}
		}
	}
	if red != 3 {
		t.Fatalf("red publish spans = %d, want 3", red)
	}

	h.lr.Fail(0)
	if _, err := h.p.attempt(context.Background(), 6, ev); err != nil {
		t.Fatal(err)
	}
	if h.lr.Len() != 1 || h.lr.Calls("create_asset", http.StatusCreated) != 1 {
		t.Fatalf("after heal: %d assets", h.lr.Len())
	}
}

// A long outage must never exhaust a photo: the demo promises nothing is lost.
func TestLongOutageNeverGivesUp(t *testing.T) {
	h := newHarness(t)
	ev := h.photo(t, "jpeg four")
	h.lr.Fail(http.StatusServiceUnavailable)

	permanent, err := h.p.attempt(context.Background(), 200, ev)
	if err == nil || permanent {
		t.Fatalf("permanent=%v err=%v, want a retry", permanent, err)
	}
	f := h.last(t).v.(events.PhotoFailed)
	if f.RetryAt == nil || f.Attempt != 200 {
		t.Fatalf("photo.failed = %+v, want retry_at", f)
	}
	if want := h.now.Add(30 * time.Second); !f.RetryAt.Equal(want) {
		t.Fatalf("retry_at = %v, want capped backoff %v", f.RetryAt, want)
	}
	if n := h.counter(t, "keeper.photos", "outcome", "failed"); n != 0 {
		t.Fatalf("keeper.photos{failed} = %d, want 0", n)
	}
}

// With an explicit limit configured, the last delivery still gives up.
func TestConfiguredLimitGivesUp(t *testing.T) {
	h := newHarness(t)
	h.p.maxDeliver = 8
	ev := h.photo(t, "jpeg five")
	h.lr.Fail(http.StatusServiceUnavailable)

	permanent, err := h.p.attempt(context.Background(), 8, ev)
	if err == nil || !permanent {
		t.Fatalf("permanent=%v err=%v, want permanent", permanent, err)
	}
	if f := h.last(t).v.(events.PhotoFailed); f.RetryAt != nil {
		t.Fatalf("photo.failed = %+v, want no retry_at", f)
	}
}

func TestConflictIsPermanent(t *testing.T) {
	h := newHarness(t)
	ev := h.photo(t, "jpeg five")
	other := sha256.Sum256([]byte("someone else's photo"))
	h.lr.Put(lightroom.AssetID(ev.SHA256), lightroomtest.Asset{SHA256: hex.EncodeToString(other[:])})

	permanent, err := h.p.attempt(context.Background(), 1, ev)
	if err == nil || !permanent {
		t.Fatalf("permanent=%v err=%v, want permanent", permanent, err)
	}
	f := h.last(t).v.(events.PhotoFailed)
	if f.ErrorClass != lightroom.ClassConflict || f.RetryAt != nil {
		t.Fatalf("photo.failed = %+v", f)
	}
	if n := h.counter(t, "keeper.publish.attempts", "result", "failed"); n != 1 {
		t.Fatalf("attempts{failed} = %d", n)
	}
}

func TestMissingSpoolFileIsRetried(t *testing.T) {
	h := newHarness(t)
	ev := h.photo(t, "jpeg six")
	if err := os.Remove(filepath.Join(os.Getenv("SPOOL_DIR"), "photos", ev.SHA256+".jpg")); err != nil {
		t.Fatal(err)
	}
	permanent, err := h.p.attempt(context.Background(), 1, ev)
	if err == nil || permanent {
		t.Fatalf("permanent=%v err=%v, want retry", permanent, err)
	}
	if f := h.last(t).v.(events.PhotoFailed); f.ErrorClass != classSpool || h.lr.Len() != 0 {
		t.Fatalf("photo.failed = %+v, %d assets", f, h.lr.Len())
	}
}

func TestInvalidEventIsTerminated(t *testing.T) {
	h := newHarness(t)
	permanent, err := h.p.attempt(context.Background(), 1, events.PhotoDecided{SHA256: "nope", Rating: 3})
	if err == nil || !permanent {
		t.Fatalf("permanent=%v err=%v", permanent, err)
	}
}

// A raw original is uploaded as the raw, under the camera's file name.
func TestRawOriginalUploadedWithNameAndType(t *testing.T) {
	h := newHarness(t)
	content := "sony raw bytes"
	sum := sha256.Sum256([]byte(content))
	sha := hex.EncodeToString(sum[:])
	path := filepath.Join(os.Getenv("SPOOL_DIR"), "photos", sha+".arw")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	ev := events.PhotoDecided{V: 1, SHA256: sha, FileName: "DSC01234.ARW", Bucket: events.BucketBest, Rating: 5}
	if _, err := h.p.attempt(context.Background(), 1, ev); err != nil {
		t.Fatal(err)
	}
	a, ok := h.lr.Get(sha[:32])
	if !ok {
		t.Fatal("no asset created")
	}
	if a.FileName != "DSC01234.ARW" || a.MasterType != "image/x-sony-arw" || a.MasterSHA256 != sha {
		t.Fatalf("asset = %+v, want the raw under its camera name", a)
	}
}
