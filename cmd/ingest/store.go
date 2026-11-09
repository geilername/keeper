package main

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// store owns the ingest schema: one row per photo, keyed by sha256 (ADR-2).
type store struct{ db *pgxpool.Pool }

const schema = `
CREATE SCHEMA IF NOT EXISTS ingest;
CREATE TABLE IF NOT EXISTS ingest.photos (
	sha256        text PRIMARY KEY,
	file_name     text        NOT NULL,
	size_bytes    bigint      NOT NULL,
	captured_at   timestamptz NOT NULL,
	camera_model  text        NOT NULL DEFAULT '',
	ingested_at   timestamptz NOT NULL DEFAULT now(),
	published_at  timestamptz,
	duplicates    int         NOT NULL DEFAULT 0
);`

func openStore(ctx context.Context, url string) (*store, error) {
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(ctx, schema); err != nil {
		db.Close()
		return nil, err
	}
	return &store{db: db}, nil
}

type photoRow struct {
	SHA256      string
	FileName    string
	SizeBytes   int64
	CapturedAt  time.Time
	CameraModel string
	Published   bool
}

// insert records a new photo. It reports created=false when the sha256 is
// already known (a duplicate upload) and returns the stored row, so the
// caller can finish an earlier attempt that crashed before publishing.
func (s *store) insert(ctx context.Context, p photoRow) (row photoRow, created bool, err error) {
	tag, err := s.db.Exec(ctx, `
		INSERT INTO ingest.photos (sha256, file_name, size_bytes, captured_at, camera_model)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (sha256) DO NOTHING`, p.SHA256, p.FileName, p.SizeBytes, p.CapturedAt, p.CameraModel)
	if err != nil {
		return photoRow{}, false, err
	}
	if tag.RowsAffected() == 1 {
		return p, true, nil
	}
	err = s.db.QueryRow(ctx, `
		UPDATE ingest.photos SET duplicates = duplicates + 1 WHERE sha256 = $1
		RETURNING file_name, size_bytes, captured_at, camera_model, published_at IS NOT NULL`,
		p.SHA256).Scan(&row.FileName, &row.SizeBytes, &row.CapturedAt, &row.CameraModel, &row.Published)
	if errors.Is(err, pgx.ErrNoRows) {
		return photoRow{}, false, errors.New("photo vanished between insert and read")
	}
	row.SHA256 = p.SHA256
	return row, false, err
}

func (s *store) markPublished(ctx context.Context, sha string) error {
	_, err := s.db.Exec(ctx,
		`UPDATE ingest.photos SET published_at = now() WHERE sha256 = $1 AND published_at IS NULL`, sha)
	return err
}
