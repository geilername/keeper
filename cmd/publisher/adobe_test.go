package main

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/geilername/keeper/internal/events"
	"github.com/geilername/keeper/internal/lightroom"
	"github.com/geilername/keeper/internal/lightroom/lightroomtest"
	"github.com/geilername/keeper/internal/lrlink"
)

// targets is a settable targetSource (the KV watcher in production).
type targets struct {
	mu sync.Mutex
	t  *lrlink.Target
}

func (s *targets) Target() (lrlink.Target, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.t == nil {
		return lrlink.Target{}, false
	}
	return *s.t, true
}

func (s *targets) set(t lrlink.Target) { s.mu.Lock(); s.t = &t; s.mu.Unlock() }

type adobeHarness struct {
	*harness
	adobe   *lightroomtest.Adobe
	targets *targets
}

// newAdobeHarness is newHarness with the Adobe client against the fake
// Partner API, and the album step on.
func newAdobeHarness(t *testing.T) *adobeHarness {
	t.Helper()
	h := newHarness(t) // spool dir, recorders; its mock server stays unused
	ah := &adobeHarness{harness: h, adobe: lightroomtest.NewAdobe("key", "tok"), targets: &targets{}}
	t.Cleanup(ah.adobe.Close)
	ah.adobe.AddAlbum(lightroomtest.Album{ID: "set1", Name: "2026", Subtype: "collection_set"})
	ah.adobe.AddAlbum(lightroomtest.Album{ID: "col1", Name: "Talks", Subtype: "collection", ParentID: "set1"})

	h.spans = tracetest.NewSpanRecorder()
	h.reader = sdkmetric.NewManualReader()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(h.spans))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(h.reader))
	lr := lightroom.NewAdobe(ah.adobe.URL, "key", lightroom.StaticToken("tok"), time.Second)
	p, err := newPublisher(lr, h.p.emit, consumerConfig, tp.Tracer("test"), mp.Meter("test"))
	if err != nil {
		t.Fatal(err)
	}
	p.now = h.p.now
	p.ratedDir = h.p.ratedDir
	h.p = p.withAlbums(lr, ah.targets)
	return ah
}

func TestAdobeNoTargetHoldsPhoto(t *testing.T) {
	h := newAdobeHarness(t)
	ev := h.photo(t, "jpeg no target")

	for delivered := 1; delivered <= 2; delivered++ {
		permanent, err := h.p.attempt(context.Background(), delivered, ev)
		if err == nil || permanent {
			t.Fatalf("delivery %d: permanent=%v err=%v, want a retry", delivered, permanent, err)
		}
		f := h.last(t).v.(events.PhotoFailed)
		if f.ErrorClass != lightroom.ClassNoTarget || f.RetryAt == nil || !strings.Contains(f.Error, "No Lightroom collection") {
			t.Fatalf("photo.failed = %+v", f)
		}
	}
	// Nothing went into the catalogue while there was nowhere to put it.
	if n := h.adobe.Calls("create_asset", http.StatusCreated); n != 0 {
		t.Fatalf("assets created without a target: %d", n)
	}
	if n := h.counter(t, "keeper.lightroom.album.adds", "result", "no_target"); n != 2 {
		t.Fatalf("album.adds{no_target} = %d", n)
	}

	// The user picks a collection; the next delivery goes through.
	h.targets.set(lrlink.Target{AlbumID: "col1", Name: "Talks", Path: "2026 / Talks"})
	if permanent, err := h.p.attempt(context.Background(), 3, ev); err != nil || permanent {
		t.Fatalf("after target: permanent=%v err=%v", permanent, err)
	}
	if !h.adobe.InAlbum("col1", lightroom.AssetID(ev.SHA256)) {
		t.Fatal("asset not in the collection")
	}
}

func TestAdobePublishAddsToCollectionIdempotently(t *testing.T) {
	h := newAdobeHarness(t)
	h.targets.set(lrlink.Target{AlbumID: "col1", Name: "Talks"})
	ev := h.photo(t, "jpeg to collection")
	id := lightroom.AssetID(ev.SHA256)

	for delivered := 1; delivered <= 2; delivered++ {
		if permanent, err := h.p.attempt(context.Background(), delivered, ev); err != nil || permanent {
			t.Fatalf("delivery %d: permanent=%v err=%v", delivered, permanent, err)
		}
		if got := h.last(t); got.subject != events.SubjectPublished {
			t.Fatalf("delivery %d emitted %+v", delivered, got)
		}
	}
	a, _ := h.adobe.Asset(id)
	if a.MasterSHA256 != ev.SHA256 || a.MasterType != "image/jpeg" || !h.adobe.InAlbum("col1", id) {
		t.Fatalf("asset %+v in album %v", a, h.adobe.InAlbum("col1", id))
	}
	// The replay saw the master via GET and skipped the upload; the add
	// found the asset already in the collection.
	if h.adobe.Calls("upload_master", 201) != 1 || h.adobe.Calls("add_to_album", 201) != 1 ||
		h.adobe.Calls("add_to_album", 403) != 1 {
		t.Fatalf("uploads %d, adds 201×%d 403×%d", h.adobe.Calls("upload_master", 201),
			h.adobe.Calls("add_to_album", 201), h.adobe.Calls("add_to_album", 403))
	}
	if n := h.counter(t, "keeper.lightroom.album.adds", "result", "ok"); n != 2 {
		t.Fatalf("album.adds{ok} = %d", n)
	}

	var album, rating int
	for _, s := range h.spans.Ended() {
		if s.Name() == "lightroom.add_to_album" {
			album++
		}
		for _, kv := range s.Attributes() {
			if kv.Key == "keeper.lightroom.rating_written" && !kv.Value.AsBool() {
				rating++
			}
		}
	}
	if album != 2 || rating != 2 {
		t.Fatalf("add_to_album spans %d, rating_written=false %d", album, rating)
	}
}

func TestAdobeRefusedCollectionIsPermanent(t *testing.T) {
	h := newAdobeHarness(t)
	h.targets.set(lrlink.Target{AlbumID: "col1", Name: "Talks"})
	h.adobe.Refuse("col1")
	ev := h.photo(t, "jpeg refused")

	permanent, err := h.p.attempt(context.Background(), 1, ev)
	if err == nil || !permanent {
		t.Fatalf("permanent=%v err=%v, want permanent", permanent, err)
	}
	f := h.last(t).v.(events.PhotoFailed)
	if f.ErrorClass != lightroom.ClassRefused || f.RetryAt != nil || f.Error != "Lightroom refused collection Talks" {
		t.Fatalf("photo.failed = %+v", f)
	}
	if n := h.counter(t, "keeper.lightroom.album.adds", "result", "refused"); n != 1 {
		t.Fatalf("album.adds{refused} = %d", n)
	}
	if n := h.counter(t, "keeper.photos", "outcome", "failed"); n != 1 {
		t.Fatalf("keeper.photos{failed} = %d", n)
	}
}

func TestAdobeNotConnectedRetries(t *testing.T) {
	h := newAdobeHarness(t)
	h.targets.set(lrlink.Target{AlbumID: "col1", Name: "Talks"})
	h.adobe.SetToken("someone-else") // our token is now rejected (403 4300)
	ev := h.photo(t, "jpeg expired")

	permanent, err := h.p.attempt(context.Background(), 1, ev)
	if err == nil || permanent {
		t.Fatalf("permanent=%v err=%v, want retry", permanent, err)
	}
	f := h.last(t).v.(events.PhotoFailed)
	if f.ErrorClass != lightroom.ClassAuth || f.RetryAt == nil || !strings.Contains(f.Error, "connect it again") {
		t.Fatalf("photo.failed = %+v", f)
	}
}
