package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/geilername/keeper/internal/events"
)

// schema is the whole read model. It is created at startup; there is no
// migration framework. The read model can always be rebuilt: drop the schema
// and delete the "gateway" consumer, and the stream replays every event.
const schema = `
CREATE SCHEMA IF NOT EXISTS gateway;

CREATE TABLE IF NOT EXISTS gateway.photos (
	sha256             text PRIMARY KEY CHECK (sha256 ~ '^[0-9a-f]{64}$'),
	file_name          text NOT NULL DEFAULT '',
	captured_at        timestamptz,
	stage              text NOT NULL DEFAULT '',
	features           jsonb,
	burst_id           text NOT NULL DEFAULT '',
	bucket             text NOT NULL DEFAULT '',
	rating             int  NOT NULL DEFAULT 0,
	reasons            text[] NOT NULL DEFAULT '{}',
	attempts           int  NOT NULL DEFAULT 0,
	last_error         text NOT NULL DEFAULT '',
	lightroom_asset_id text NOT NULL DEFAULT '',
	seen_at            timestamptz NOT NULL,
	ingested_at        timestamptz,
	scored_at          timestamptz,
	decided_at         timestamptz,
	published_at       timestamptz,
	failed_at          timestamptz,
	updated_at         timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE gateway.photos ADD COLUMN IF NOT EXISTS retry_at timestamptz;
CREATE INDEX IF NOT EXISTS photos_seen_at_idx ON gateway.photos (seen_at DESC, sha256);
CREATE INDEX IF NOT EXISTS photos_burst_id_idx ON gateway.photos (burst_id) WHERE burst_id <> '';

CREATE TABLE IF NOT EXISTS gateway.bursts (
	burst_id     text PRIMARY KEY,
	members      text[] NOT NULL,
	best         text NOT NULL,
	reason       text NOT NULL DEFAULT '',
	close_reason text NOT NULL DEFAULT '',
	decided_at   timestamptz NOT NULL,
	updated_at   timestamptz NOT NULL DEFAULT now()
);
`

const photoCols = `sha256, file_name, captured_at, stage, features, burst_id, bucket, rating,
	reasons, attempts, last_error, lightroom_asset_id, seen_at, ingested_at, scored_at,
	decided_at, published_at, failed_at, retry_at, updated_at`

// Store is the gateway's Postgres read model.
type Store struct{ pool *pgxpool.Pool }

// OpenStore connects with a small pool (retrying until ctx ends, Postgres may
// still be starting) and creates the schema.
func OpenStore(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	for {
		_, err = pool.Exec(ctx, schema)
		if err == nil {
			return &Store{pool: pool}, nil
		}
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, fmt.Errorf("create schema: %w", err)
		case <-time.After(time.Second):
		}
	}
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// ApplyPhotoEvent folds ev into the photo's row inside one transaction.
// changed is false when the event did not change anything (a redelivery);
// then nothing is written and no SSE update is due.
//
// The placeholder INSERT … ON CONFLICT DO NOTHING guarantees a row exists,
// so SELECT … FOR UPDATE always locks it: two concurrent events for the same
// new photo cannot both start from an empty photo.
func (s *Store) ApplyPhotoEvent(ctx context.Context, sha string, ev any, at time.Time,
) (p Photo, changed, firstPublished bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return p, false, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit

	if _, err = tx.Exec(ctx,
		`INSERT INTO gateway.photos (sha256, seen_at) VALUES ($1, $2) ON CONFLICT (sha256) DO NOTHING`,
		sha, normTime(at)); err != nil {
		return p, false, false, err
	}
	before, err := scanPhoto(tx.QueryRow(ctx,
		`SELECT `+photoCols+` FROM gateway.photos WHERE sha256 = $1 FOR UPDATE`, sha))
	if err != nil {
		return p, false, false, err
	}

	after, firstPublished := apply(before, ev, at)
	if reflect.DeepEqual(before, after) {
		return before, false, false, tx.Commit(ctx)
	}

	err = tx.QueryRow(ctx, `
		UPDATE gateway.photos SET
			file_name = $2, captured_at = $3, stage = $4, features = $5, burst_id = $6,
			bucket = $7, rating = $8, reasons = $9, attempts = $10, last_error = $11,
			lightroom_asset_id = $12, seen_at = $13, ingested_at = $14, scored_at = $15,
			decided_at = $16, published_at = $17, failed_at = $18, retry_at = $19, updated_at = now()
		WHERE sha256 = $1
		RETURNING updated_at`,
		sha, after.FileName, after.CapturedAt, after.Stage, after.Features, after.BurstID,
		after.Bucket, after.Rating, after.Reasons, after.Attempts, after.LastError,
		after.LightroomAssetID, after.SeenAt, after.IngestedAt, after.ScoredAt,
		after.DecidedAt, after.PublishedAt, after.FailedAt, after.RetryAt,
	).Scan(&after.UpdatedAt)
	if err != nil {
		return p, false, false, err
	}
	after.UpdatedAt = after.UpdatedAt.UTC()
	if err = tx.Commit(ctx); err != nil {
		return p, false, false, err
	}
	return after, true, firstPublished, nil
}

// UpsertBurst stores a burst decision. Redelivery writes the same values.
func (s *Store) UpsertBurst(ctx context.Context, b *events.BurstDecided, at time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO gateway.bursts (burst_id, members, best, reason, close_reason, decided_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (burst_id) DO UPDATE SET
			members = excluded.members, best = excluded.best, reason = excluded.reason,
			close_reason = excluded.close_reason,
			decided_at = LEAST(gateway.bursts.decided_at, excluded.decided_at),
			updated_at = now()`,
		b.BurstID, b.Members, b.Best, b.Reason, b.CloseReason, normTime(at))
	return err
}

// ListPhotos returns the newest photos first (by the time the pipeline first
// reported them, which does not change as the photo moves on).
func (s *Store) ListPhotos(ctx context.Context, limit int) ([]Photo, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+photoCols+` FROM gateway.photos ORDER BY seen_at DESC, sha256 LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return collectPhotos(rows)
}

// ErrNotFound is returned when a photo or burst does not exist.
var ErrNotFound = errors.New("not found")

func (s *Store) GetPhoto(ctx context.Context, sha string) (Photo, error) {
	p, err := scanPhoto(s.pool.QueryRow(ctx,
		`SELECT `+photoCols+` FROM gateway.photos WHERE sha256 = $1`, sha))
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// BurstPhotos returns the photos of a burst in capture order.
func (s *Store) BurstPhotos(ctx context.Context, burstID string) ([]Photo, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+photoCols+` FROM gateway.photos WHERE burst_id = $1
		 ORDER BY captured_at NULLS LAST, sha256`, burstID)
	if err != nil {
		return nil, err
	}
	return collectPhotos(rows)
}

// Burst is one row of gateway.bursts.
type Burst struct {
	BurstID     string    `json:"burst_id"`
	Members     []string  `json:"members"`
	Best        string    `json:"best"`
	Reason      string    `json:"reason"`
	CloseReason string    `json:"close_reason"`
	DecidedAt   time.Time `json:"decided_at"`
}

func (s *Store) GetBurst(ctx context.Context, id string) (Burst, error) {
	var b Burst
	err := s.pool.QueryRow(ctx, `
		SELECT burst_id, members, best, reason, close_reason, decided_at
		FROM gateway.bursts WHERE burst_id = $1`, id,
	).Scan(&b.BurstID, &b.Members, &b.Best, &b.Reason, &b.CloseReason, &b.DecidedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, ErrNotFound
	}
	b.DecidedAt = b.DecidedAt.UTC()
	return b, err
}

func collectPhotos(rows pgx.Rows) ([]Photo, error) {
	defer rows.Close()
	out := []Photo{}
	for rows.Next() {
		p, err := scanPhoto(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func scanPhoto(row pgx.Row) (Photo, error) {
	var p Photo
	err := row.Scan(&p.SHA256, &p.FileName, &p.CapturedAt, &p.Stage, &p.Features, &p.BurstID,
		&p.Bucket, &p.Rating, &p.Reasons, &p.Attempts, &p.LastError, &p.LightroomAssetID,
		&p.SeenAt, &p.IngestedAt, &p.ScoredAt, &p.DecidedAt, &p.PublishedAt, &p.FailedAt,
		&p.RetryAt, &p.UpdatedAt)
	if err != nil {
		return p, err
	}
	// pgx returns times in the local zone; normalise so apply's output
	// compares equal to what was read.
	for _, t := range []**time.Time{&p.CapturedAt, &p.IngestedAt, &p.ScoredAt,
		&p.DecidedAt, &p.PublishedAt, &p.FailedAt} {
		if *t != nil {
			*t = ptr((**t).UTC())
		}
	}
	p.SeenAt = p.SeenAt.UTC()
	p.UpdatedAt = p.UpdatedAt.UTC()
	if p.Reasons == nil {
		p.Reasons = []string{}
	}
	return p, nil
}
