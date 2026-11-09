package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/geilername/keeper/internal/events"
	"github.com/geilername/keeper/internal/media"
	"github.com/geilername/keeper/internal/natsx"
	"github.com/geilername/keeper/internal/spool"
	"github.com/geilername/keeper/internal/telemetry"
)

// hookPayload is what the SFTPGo EventManager HTTP action posts on upload.
type hookPayload struct {
	Event       string          `json:"event"`
	FSPath      string          `json:"fs_path"`
	VirtualPath string          `json:"virtual_path"`
	FileSize    json.Number     `json:"file_size"`
	User        string          `json:"user"`
	Timestamp   json.RawMessage `json:"timestamp"`
}

type ingester struct {
	store    *store
	js       jetstream.JetStream
	tracer   trace.Tracer
	photos   metric.Int64Counter
	duration metric.Float64Histogram
}

func newIngester(st *store, js jetstream.JetStream) *ingester {
	m := otel.Meter(service)
	photos, _ := m.Int64Counter("keeper.photos",
		metric.WithDescription("Photos by pipeline outcome"))
	duration, _ := m.Float64Histogram("keeper.stage.duration", metric.WithUnit("s"),
		metric.WithDescription("Time spent per pipeline stage"))
	return &ingester{store: st, js: js, tracer: otel.Tracer(service), photos: photos, duration: duration}
}

type result struct {
	SHA256  string `json:"sha256"`
	Outcome string `json:"outcome"` // ingested | duplicate | resumed
}

// hook handles one SFTPGo upload notification. 2xx means "done, don't
// retry" (also for duplicates and ignored files); 5xx means "retry".
func (in *ingester) hook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var h hookPayload
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&h); err != nil {
		http.Error(w, "bad hook payload", http.StatusBadRequest)
		return
	}
	if h.Event != "upload" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	path, err := incomingPath(h.FSPath)
	if err != nil {
		slog.WarnContext(ctx, "rejected hook path", "fs_path", h.FSPath, "err", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	mt, ok := media.TypeOf(path)
	if !ok {
		slog.InfoContext(ctx, "ignoring upload that is neither JPEG nor ARW", "file", filepath.Base(path))
		w.WriteHeader(http.StatusNoContent)
		return
	}

	res, err := in.ingest(ctx, path, mt)
	if err != nil {
		slog.ErrorContext(ctx, "ingest failed", "file", filepath.Base(path), "err", err)
		http.Error(w, "ingest failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

// incomingPath only accepts regular paths inside the spool's incoming dir,
// whatever the hook says.
func incomingPath(p string) (string, error) {
	if p == "" {
		return "", errors.New("fs_path missing")
	}
	clean := filepath.Clean(p)
	rel, err := filepath.Rel(spool.Incoming(), clean)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("fs_path outside %s", spool.Incoming())
	}
	return clean, nil
}

func (in *ingester) ingest(ctx context.Context, path string, mt media.Type) (result, error) {
	start := time.Now()
	hookSpan := trace.SpanFromContext(ctx).SpanContext()

	sha, size, err := hashFile(path)
	if err != nil {
		return result{}, err
	}
	hashed := time.Now()
	meta := readMeta(path)

	// Keep the bytes content-addressed before anything else refers to them.
	if err := linkOrCopy(path, spool.OriginalPath(sha, mt.Ext())); err != nil {
		return result{}, fmt.Errorf("store original: %w", err)
	}

	row, created, err := in.store.insert(ctx, photoRow{
		SHA256: sha, FileName: filepath.Base(path), SizeBytes: size,
		CapturedAt: meta.capturedAt, CameraModel: meta.model,
	})
	if err != nil {
		return result{}, fmt.Errorf("record photo: %w", err)
	}

	links := []trace.Link{{SpanContext: hookSpan}}
	attrs := []attribute.KeyValue{
		attribute.String("keeper.sha256", sha),
		attribute.String("keeper.file_name", row.FileName),
		attribute.Int64("keeper.size_bytes", size),
		attribute.String("keeper.media_type", string(mt)),
	}

	if !created {
		// Known photo: never a second root span, just a note in its trace.
		outcome := "duplicate"
		if !row.Published {
			outcome = "resumed" // an earlier attempt stopped before publishing
		}
		pctx, span := in.tracer.Start(telemetry.PhotoRootContext(ctx, sha), "ingest "+outcome,
			trace.WithLinks(links...), trace.WithAttributes(append(attrs,
				attribute.String("keeper.outcome", outcome))...))
		defer span.End()
		if outcome == "resumed" {
			if err := in.finish(pctx, row); err != nil {
				return result{}, fail(span, err)
			}
		}
		removeIncoming(pctx, path)
		in.photos.Add(pctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
		slog.InfoContext(pctx, "photo already known", "outcome", outcome, "sha256", sha, "file", row.FileName)
		return result{SHA256: sha, Outcome: outcome}, nil
	}

	// New photo: the root span of its trace, with derived IDs (ADR-6).
	pctx, root := in.tracer.Start(telemetry.WithPhotoRoot(ctx, sha), "photo "+row.FileName,
		trace.WithNewRoot(), trace.WithTimestamp(start), trace.WithLinks(links...),
		trace.WithAttributes(append(attrs, attribute.String("keeper.outcome", "ingested"))...))
	defer root.End()
	// Hashing happened before the trace ID was known: rebuild its span.
	_, hs := in.tracer.Start(pctx, "hash", trace.WithTimestamp(start))
	hs.End(trace.WithTimestamp(hashed))

	if err := in.finish(pctx, row); err != nil {
		return result{}, fail(root, err)
	}
	removeIncoming(pctx, path)
	in.photos.Add(pctx, 1, metric.WithAttributes(attribute.String("outcome", "ingested")))
	in.duration.Record(pctx, time.Since(start).Seconds(),
		metric.WithAttributes(attribute.String("stage", "ingest")))
	slog.InfoContext(pctx, "photo ingested", "sha256", sha, "file", row.FileName,
		"captured_at", row.CapturedAt, "camera", row.CameraModel)
	return result{SHA256: sha, Outcome: "ingested"}, nil
}

// finish makes the preview and thumbnail and publishes photo.ingested. Safe
// to repeat: existing files are kept and JetStream drops a duplicate
// Nats-Msg-Id.
func (in *ingester) finish(ctx context.Context, row photoRow) error {
	mt, _ := media.TypeOf(row.FileName)
	original := spool.OriginalPath(row.SHA256, mt.Ext())
	preview := spool.PreviewPath(row.SHA256)

	_, span := in.tracer.Start(ctx, "preview", trace.WithAttributes(attribute.String("keeper.media_type", string(mt))))
	err := makePreview(original, mt, readMeta(original).orientation, preview)
	if err != nil {
		// Nothing downstream can score this photo: fail the hook loudly.
		err = fail(span, fmt.Errorf("preview: %w", err))
		span.End()
		return err
	}
	span.End()

	if _, err := os.Stat(spool.ThumbPath(row.SHA256)); err != nil {
		tctx, span := in.tracer.Start(ctx, "thumbnail")
		if err := makeThumbnail(preview, spool.ThumbPath(row.SHA256)); err != nil {
			// The console can live without a thumbnail; the pipeline goes on.
			span.RecordError(err)
			slog.WarnContext(tctx, "thumbnail failed", "err", err)
		}
		span.End()
	}
	ev := events.PhotoIngested{
		V: events.Version, SHA256: row.SHA256, FileName: row.FileName,
		Path: preview, OriginalPath: original, MediaType: string(mt),
		SizeBytes: row.SizeBytes, CapturedAt: row.CapturedAt, CameraModel: row.CameraModel,
	}
	if err := natsx.Publish(ctx, in.js, events.SubjectIngested,
		events.MsgID(events.SubjectIngested, row.SHA256), ev); err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	return in.store.markPublished(ctx, row.SHA256)
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// linkOrCopy places src at dst unless dst exists. A hard link is instant
// on the shared volume; copying is the fallback.
func linkOrCopy(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Link(src, dst); err == nil || errors.Is(err, os.ErrExist) {
		return nil
	}
	return copyAtomic(src, dst)
}

func copyAtomic(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil { // CreateTemp makes 0600
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

func removeIncoming(ctx context.Context, path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.WarnContext(ctx, "could not remove incoming file", "err", err)
	}
}

func fail(span trace.Span, err error) error {
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	return err
}
