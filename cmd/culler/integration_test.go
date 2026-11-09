package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/geilername/keeper/internal/events"
	"github.com/geilername/keeper/internal/natsx"
)

// Integration tests against a throwaway JetStream (they use the real
// "culler" durable):
//
//	docker run -d --rm --name culler-nats -p 14222:4222 nats:2.15.2-alpine -js
//	NATS_URL=nats://127.0.0.1:14222 go test -race -v -run Integration ./cmd/culler/

func connect(t *testing.T) (*nats.Conn, jetstream.JetStream) {
	t.Helper()
	url := os.Getenv("NATS_URL")
	if url == "" {
		t.Skip("NATS_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	nc, js, err := natsx.Connect(ctx, url, "culler-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	return nc, js
}

// demoBurst publishes 6 frames shot 300 ms apart: one best, two keepers,
// three rejects (blink, motion blur, too dark).
func demoBurst(t *testing.T, js jetstream.JetStream, tag string) []string {
	t.Helper()
	base := time.Now().UTC()
	type f struct {
		sharp, expo, mean float64
		eyes              string
	}
	frames := []f{
		{0.74, 0.7, 0.5, events.EyesOpen},   // keeper
		{0.88, 0.6, 0.5, events.EyesClosed}, // reject: eyes closed
		{0.83, 0.7, 0.5, events.EyesOpen},   // best
		{0.21, 0.7, 0.5, events.EyesOpen},   // reject: motion blur
		{0.79, 0.7, 0.5, events.EyesOpen},   // keeper
		{0.80, 0.2, 0.1, events.EyesOpen},   // reject: underexposed
	}
	var shas []string
	for i, fr := range frames {
		sha := shaOf(fmt.Sprintf("%s-%d-%d", tag, base.UnixNano(), i))
		shas = append(shas, sha)
		ev := events.PhotoScored{
			V: events.Version, SHA256: sha, CapturedAt: base.Add(time.Duration(i) * 300 * time.Millisecond),
			Features: events.Features{
				Sharpness: events.Sharpness{Score: fr.sharp},
				Exposure:  events.Exposure{Score: fr.expo, Mean: fr.mean},
				Eyes:      events.Eyes{State: fr.eyes, Faces: 1},
				DHash:     []string{hashA, hashNearA}[i%2],
			},
		}
		if err := natsx.Publish(context.Background(), js, events.SubjectScored,
			events.MsgID(events.SubjectScored, sha), ev); err != nil {
			t.Fatal(err)
		}
	}
	return shas
}

// collect gathers decisions for our photos until want arrived, then waits
// a little longer to catch duplicates.
func collect(t *testing.T, sub *nats.Subscription, shas []string, timeout time.Duration) map[string][]events.PhotoDecided {
	t.Helper()
	ours := map[string]bool{}
	for _, s := range shas {
		ours[s] = true
	}
	got := map[string][]events.PhotoDecided{}
	n := 0
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		m, err := sub.NextMsg(200 * time.Millisecond)
		if err != nil {
			continue
		}
		var d events.PhotoDecided
		if json.Unmarshal(m.Data, &d) == nil && ours[d.SHA256] {
			got[d.SHA256] = append(got[d.SHA256], d)
			n++
			if n == len(shas) && time.Until(deadline) > 3*time.Second {
				deadline = time.Now().Add(3 * time.Second)
			}
		}
	}
	return got
}

func checkDemoDecisions(t *testing.T, got map[string][]events.PhotoDecided, shas []string) {
	t.Helper()
	wantBuckets := []string{
		events.BucketKeeper, events.BucketReject, events.BucketBest,
		events.BucketReject, events.BucketKeeper, events.BucketReject,
	}
	for i, sha := range shas {
		ds := got[sha]
		if len(ds) != 1 {
			t.Errorf("frame %d: %d decisions, want exactly 1", i, len(ds))
			continue
		}
		d := ds[0]
		if d.Bucket != wantBuckets[i] || d.BurstID != shas[0] {
			t.Errorf("frame %d: %s in burst %s…, want %s in %s…", i, d.Bucket, d.BurstID[:8], wantBuckets[i], shas[0][:8])
		}
		t.Logf("frame %d  %d★ %-6s %q", i, d.Rating, d.Bucket, d.Reasons)
	}
}

func waitNoPending(t *testing.T, js jetstream.JetStream) {
	t.Helper()
	ctx := context.Background()
	for range 20 {
		c, err := js.Consumer(ctx, events.Stream, service)
		if err == nil {
			if info, err := c.Info(ctx); err == nil && info.NumAckPending == 0 {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Error("held messages were not all acked")
}

func TestIntegrationBurstClosesOnIdle(t *testing.T) {
	nc, js := connect(t)
	sub, err := nc.SubscribeSync(events.SubjectDecided)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Unsubscribe()

	ctx, cancel := context.WithCancel(context.Background())
	cfg := windowConfig{Gap: 2 * time.Second, MaxHamming: 12, Idle: 2 * time.Second, MaxWait: 30 * time.Second}
	served := make(chan error, 1)
	go func() { served <- serve(ctx, js, cfg, 6*time.Second) }()

	shas := demoBurst(t, js, t.Name())
	start := time.Now()
	got := collect(t, sub, shas, 20*time.Second)
	t.Logf("decided %d photos, first after ~%s", len(got), time.Since(start).Round(time.Second))
	checkDemoDecisions(t, got, shas)
	waitNoPending(t, js)

	cancel()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}

// A culler that stops with an open window leaves it unacked; the next one
// gets the photos redelivered, rebuilds the window and decides it once.
func TestIntegrationRestartRebuildsWindow(t *testing.T) {
	nc, js := connect(t)
	sub, err := nc.SubscribeSync(events.SubjectDecided)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Unsubscribe()

	cfg := windowConfig{Gap: 2 * time.Second, MaxHamming: 12, Idle: 3 * time.Second, MaxWait: 30 * time.Second}
	const ackWait = 3 * time.Second

	ctx1, stop1 := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serve(ctx1, js, cfg, ackWait) }()
	shas := demoBurst(t, js, t.Name())
	time.Sleep(time.Second) // held, window open, not idle yet
	stop1()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	if early := collect(t, sub, shas, time.Second); len(early) != 0 {
		t.Fatalf("decided before the window closed: %d", len(early))
	}

	ctx2, stop2 := context.WithCancel(context.Background())
	defer stop2()
	go func() { served <- serve(ctx2, js, cfg, ackWait) }()
	got := collect(t, sub, shas, 20*time.Second)
	checkDemoDecisions(t, got, shas)
	waitNoPending(t, js)
	stop2()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}
