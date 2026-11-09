package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/geilername/keeper/internal/events"
	"github.com/geilername/keeper/internal/natsx"
	"github.com/geilername/keeper/internal/photostate"
	"github.com/geilername/keeper/internal/search"
)

// durable is the indexer's JetStream consumer. Deleting it (or the index,
// see ensureIndex) replays the whole stream into Elasticsearch.
const durable = "indexer"

// maxConflicts bounds the read-modify-write retries for one event. Two
// writers on one photo resolve in a round or two; more means something is
// wrong, and the message goes back to JetStream instead.
const maxConflicts = 5

// docStore is the part of search.Client the indexer uses.
type docStore interface {
	Get(ctx context.Context, index, id string, v any) (search.Version, bool, error)
	Put(ctx context.Context, index, id string, v any, ver *search.Version) error
}

type indexer struct {
	es  docStore
	now func() time.Time
}

// ensureIndex waits for Elasticsearch and creates the photos index. When it
// had to create the index (first start, or someone deleted it to rebuild),
// it also deletes the durable, so the next consume starts at the beginning
// of the stream and refills the index from history.
func ensureIndex(ctx context.Context, es *search.Client, js jetstream.JetStream) error {
	for {
		created, err := es.EnsureIndex(ctx, search.PhotosIndex, search.PhotosMapping)
		if err == nil {
			if !created {
				return nil
			}
			slog.InfoContext(ctx, "created index, replaying the stream", "index", search.PhotosIndex)
			err = js.DeleteConsumer(ctx, events.Stream, durable)
			if err == nil || errors.Is(err, jetstream.ErrConsumerNotFound) ||
				errors.Is(err, jetstream.ErrStreamNotFound) {
				return nil
			}
			return fmt.Errorf("reset consumer %s: %w", durable, err)
		}
		slog.WarnContext(ctx, "elasticsearch not ready", "err", err)
		select {
		case <-ctx.Done():
			return fmt.Errorf("create index: %w", err)
		case <-time.After(2 * time.Second):
		}
	}
}

// run consumes every photo event until ctx ends. Like the gateway's read
// model, the index must not lose events: while Elasticsearch is down,
// messages keep being redelivered with backoff instead of giving up.
func (ix *indexer) run(ctx context.Context, js jetstream.JetStream) error {
	return natsx.Consume(ctx, js, natsx.ConsumerConfig{
		Durable:       durable,
		FilterSubject: "photo.>",
		MaxDeliver:    -1,
	}, rawSHA, ix.handle)
}

// rawSHA lets natsx put a message without traceparent into its photo trace.
func rawSHA(raw json.RawMessage) string {
	var v struct {
		SHA256 string `json:"sha256"`
	}
	_ = json.Unmarshal(raw, &v)
	return v.SHA256
}

func (ix *indexer) handle(ctx context.Context, m natsx.Msg, raw json.RawMessage) error {
	// The stream timestamp is the event time: stable across redeliveries.
	at := ix.now()
	if md, err := m.Metadata(); err == nil {
		at = md.Timestamp
	}
	sha, ev, err := photostate.Decode(m.Subject(), raw)
	if err != nil {
		return natsx.Permanent(err)
	}
	if ev == nil {
		slog.InfoContext(ctx, "ignoring unknown subject", "subject", m.Subject())
		return nil
	}
	return ix.apply(ctx, sha, ev, at)
}

// apply folds ev into the photo's document: read it, apply the same fold as
// the gateway, and write it back only if nobody wrote in between (seq_no
// check). A conflict starts over from the newer document, so concurrent or
// redelivered events never overwrite each other, and the stage only moves
// forward.
func (ix *indexer) apply(ctx context.Context, sha string, ev any, at time.Time) error {
	for range maxConflicts {
		var before photostate.Photo
		ver, found, err := ix.es.Get(ctx, search.PhotosIndex, sha, &before)
		if err != nil {
			return classify(err)
		}
		if !found {
			before = photostate.Photo{SHA256: sha, Reasons: []string{}}
		}

		after, _ := photostate.Apply(before, ev, at)
		if found && reflect.DeepEqual(before, after) {
			return nil // redelivery: nothing changed
		}
		after.UpdatedAt = photostate.NormTime(ix.now())

		var cond *search.Version
		if found {
			cond = &ver
		}
		err = ix.es.Put(ctx, search.PhotosIndex, sha, after, cond)
		if errors.Is(err, search.ErrConflict) {
			continue
		}
		return classify(err)
	}
	return fmt.Errorf("photo %s: %d version conflicts in a row", sha, maxConflicts)
}

// classify turns a request Elasticsearch rejected as invalid (a 4xx other
// than 409/429) into a permanent failure: redelivering the same document
// cannot fix it. Everything else is retried.
func classify(err error) error {
	var e *search.Error
	if errors.As(err, &e) && !e.Retryable() {
		return natsx.Permanent(err)
	}
	return err
}
