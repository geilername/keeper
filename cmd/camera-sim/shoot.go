package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/jlaffaye/ftp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// ftpTarget is where the "camera" sends its frames (SFTPGo in compose).
type ftpTarget struct {
	Addr, User, Password string
}

// shooter replays a burst set over FTP, one frame per interval.
type shooter struct {
	setsDir  string
	target   ftpTarget
	interval time.Duration
	attempts int // per frame, >= 1
	tracer   trace.Tracer

	// One burst at a time: two interleaved bursts would look like one long
	// burst to the culler and spoil the demo.
	busy sync.Mutex
}

type uploaded struct {
	Remote string `json:"remote"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type shootResult struct {
	Set        string     `json:"set"`
	Uploaded   []uploaded `json:"uploaded"`
	DurationMS int64      `json:"duration_ms"`
	TraceID    string     `json:"trace_id"`
}

// shoot uploads every frame of a set. It stops at the first failed upload;
// the frames already sent stay sent, like on a real camera.
func (s *shooter) shoot(ctx context.Context, set string, dup bool) (shootResult, error) {
	frames, err := setFrames(s.setsDir, set)
	if err != nil {
		return shootResult{}, err
	}
	plan := planUploads(frames, dup)

	ctx, span := s.tracer.Start(ctx, "shoot "+set, trace.WithAttributes(
		attribute.String("keeper.camera.set", set),
		attribute.Int("keeper.camera.frames", len(plan)),
		attribute.Bool("keeper.camera.dup", dup),
	))
	defer span.End()

	start := time.Now()
	res := shootResult{Set: set, TraceID: span.SpanContext().TraceID().String()}
	slog.InfoContext(ctx, "shoot started", "set", set, "frames", len(plan), "dup", dup)

	var conn *ftp.ServerConn
	defer func() {
		if conn != nil {
			_ = conn.Quit()
		}
	}()

	for i, u := range plan {
		if i > 0 {
			if err := sleep(ctx, s.interval); err != nil {
				span.SetStatus(codes.Error, "cancelled")
				return res, err
			}
		}
		up, err := s.uploadWithRetry(ctx, &conn, i, u)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "upload failed")
			slog.ErrorContext(ctx, "upload failed, burst aborted", "set", set, "remote", u.Remote, "err", err)
			return res, err
		}
		res.Uploaded = append(res.Uploaded, up)
	}

	res.DurationMS = time.Since(start).Milliseconds()
	slog.InfoContext(ctx, "shoot finished", "set", set, "frames", len(res.Uploaded), "duration_ms", res.DurationMS)
	return res, nil
}

func (s *shooter) dial(ctx context.Context) (*ftp.ServerConn, error) {
	ctx, span := s.tracer.Start(ctx, "ftp connect", trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("server.address", s.target.Addr)))
	defer span.End()

	// Passive mode, with the data connection going to the same host as the
	// control connection (the library ignores the IP in PASV replies), so
	// SFTPGo's advertised passive IP never matters inside compose.
	conn, err := ftp.Dial(s.target.Addr, ftp.DialWithContext(ctx), ftp.DialWithTimeout(5*time.Second))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "dial failed")
		return nil, fmt.Errorf("ftp dial %s: %w", s.target.Addr, err)
	}
	if err := conn.Login(s.target.User, s.target.Password); err != nil {
		_ = conn.Quit()
		span.RecordError(err)
		span.SetStatus(codes.Error, "login failed")
		return nil, fmt.Errorf("ftp login as %s: %w", s.target.User, err)
	}
	return conn, nil
}

// uploadWithRetry re-sends a frame the way a camera does when the server
// rejects it. SFTPGo cannot retry its upload hook: when ingest answers 5xx,
// SFTPGo deletes the file and fails the transfer with 550, so the retry has
// to come from the client. Each attempt is its own span; the connection is
// re-dialled after a failure in case it broke.
func (s *shooter) uploadWithRetry(ctx context.Context, conn **ftp.ServerConn, i int, u upload) (uploaded, error) {
	var err error
	for attempt := 1; ; attempt++ {
		if *conn == nil {
			if *conn, err = s.dial(ctx); err != nil {
				*conn = nil
			}
		}
		if *conn != nil {
			var up uploaded
			if up, err = s.uploadOne(ctx, *conn, i, attempt, u); err == nil {
				return up, nil
			}
			_ = (*conn).Quit()
			*conn = nil
		}
		if attempt >= s.attempts {
			return uploaded{}, err
		}
		slog.WarnContext(ctx, "upload attempt failed, retrying", "remote", u.Remote, "attempt", attempt, "err", err)
		if err := sleep(ctx, time.Duration(attempt)*time.Second); err != nil {
			return uploaded{}, err
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *shooter) uploadOne(ctx context.Context, conn *ftp.ServerConn, i, attempt int, u upload) (uploaded, error) {
	ctx, span := s.tracer.Start(ctx, "upload "+u.Remote, trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("file.name", u.Remote),
			attribute.Int("keeper.camera.frame_index", i),
			attribute.Int("keeper.camera.attempt", attempt),
		))
	defer span.End()

	data, err := os.ReadFile(u.Local)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "read failed")
		return uploaded{}, err
	}
	sum := sha256.Sum256(data)
	up := uploaded{Remote: u.Remote, Bytes: len(data), SHA256: hex.EncodeToString(sum[:])}
	// The sha256 is the photo ID (ADR-2): searching for it in Dash0 joins
	// this upload span to the photo's own trace.
	span.SetAttributes(
		attribute.Int("file.size", up.Bytes),
		attribute.String("keeper.photo.sha256", up.SHA256),
	)

	if err := conn.Stor(u.Remote, bytes.NewReader(data)); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "stor failed")
		return up, fmt.Errorf("ftp stor %s: %w", u.Remote, err)
	}
	slog.InfoContext(ctx, "frame uploaded", "remote", u.Remote, "bytes", up.Bytes, "sha256", up.SHA256)
	return up, nil
}
