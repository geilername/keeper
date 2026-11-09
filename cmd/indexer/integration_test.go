package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/geilername/keeper/internal/events"
	"github.com/geilername/keeper/internal/natsx"
	"github.com/geilername/keeper/internal/photostate"
	"github.com/geilername/keeper/internal/search"
)

// Integration test against a throwaway Elasticsearch and JetStream. It
// deletes and recreates the keeper-photos index and the "indexer" durable:
//
//	docker run -d --rm --name es-test -p 19200:9200 -e discovery.type=single-node \
//	  -e xpack.security.enabled=false -e ES_JAVA_OPTS="-Xms512m -Xmx512m" elasticsearch:9.5.3
//	docker run -d --rm --name nats-test -p 14223:4222 nats:2.15.2-alpine -js
//	ELASTICSEARCH_URL=http://127.0.0.1:19200 NATS_URL=nats://127.0.0.1:14223 \
//	  go test -race -v -run Integration ./cmd/indexer/
func TestIntegrationStreamToIndex(t *testing.T) {
	esURL, natsURL := os.Getenv("ELASTICSEARCH_URL"), os.Getenv("NATS_URL")
	if esURL == "" || natsURL == "" {
		t.Skip("ELASTICSEARCH_URL or NATS_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	es := search.New(esURL, 10*time.Second)
	deleteIndex(t, esURL)
	nc, js, err := natsx.Connect(ctx, natsURL, "indexer-test")
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	// Events already in the stream before the index exists must be indexed:
	// creating the index resets the durable to the start of the stream.
	sha := "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	publish(t, ctx, js, events.SubjectIngested, sha, events.PhotoIngested{V: 1, SHA256: sha,
		FileName: "DSC0042.ARW", CapturedAt: time.Now().UTC()})
	publish(t, ctx, js, events.SubjectScored, sha, events.PhotoScored{V: 1, SHA256: sha,
		Features: events.Features{Sharpness: events.Sharpness{Score: 0.11},
			Eyes: events.Eyes{State: events.EyesClosed, Faces: 1}}})
	publish(t, ctx, js, events.SubjectDecided, sha, events.PhotoDecided{V: 1, SHA256: sha,
		BurstID: "b42", Bucket: events.BucketReject, Rating: 1,
		Reasons: []string{"eyes closed", "motion blurred (sharpness 0.11, sharpest in burst 0.59)"}})

	if err := ensureIndex(ctx, es, js); err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- (&indexer{es: es, now: time.Now}).run(runCtx, js) }()
	defer func() { stop(); <-done }()

	var p photostate.Photo
	for {
		_, found, err := es.Get(ctx, search.PhotosIndex, sha, &p)
		if err != nil {
			t.Fatal(err)
		}
		if found && p.Stage == photostate.StageDecided {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("photo not indexed as decided: found=%v %+v", found, p)
		case <-time.After(200 * time.Millisecond):
		}
	}
	if p.FileName != "DSC0042.ARW" || p.Features == nil || p.BurstID != "b42" {
		t.Errorf("document = %+v", p)
	}

	refresh(t, esURL)
	for _, q := range []string{
		"blur",                       // english stemming: "blurred" in the reasons
		`"eyes closed"`,              // phrase
		"features.eyes.state:closed", // field
		"features.sharpness.score:<0.5 AND rating:1", // ranges
		"file_name.text:DSC0042.ARW",
	} {
		res, err := es.Search(ctx, search.PhotosIndex, map[string]any{
			"query": map[string]any{"query_string": map[string]any{
				"query": q, "fields": []string{"reasons", "file_name.text"}, "default_operator": "AND"}},
		})
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if res.Total != 1 {
			t.Errorf("query %s: total = %d, want 1", q, res.Total)
		}
	}
}

func publish(t *testing.T, ctx context.Context, js jetstream.JetStream, subject, sha string, v any) {
	t.Helper()
	if err := natsx.Publish(ctx, js, subject, events.MsgID(subject, sha+time.Now().String()), v); err != nil {
		t.Fatal(err)
	}
}

func deleteIndex(t *testing.T, esURL string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, esURL+"/"+search.PhotosIndex, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func refresh(t *testing.T, esURL string) {
	t.Helper()
	resp, err := http.Post(esURL+"/"+search.PhotosIndex+"/_refresh", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	_ = json.NewDecoder(resp.Body).Decode(&v)
	resp.Body.Close()
}
