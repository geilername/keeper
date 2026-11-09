package search

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func serve(t *testing.T, status int, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, time.Second)
}

func TestErrorUsesRootCause(t *testing.T) {
	c := serve(t, 400, `{"error":{"type":"search_phase_execution_exception","reason":"all shards failed",
		"root_cause":[{"type":"query_shard_exception","reason":"Failed to parse query [\"x]"}]}}`)
	_, err := c.Search(context.Background(), PhotosIndex, map[string]any{})
	var e *Error
	if !errors.As(err, &e) || e.Status != 400 || e.Type != "search_phase_execution_exception" ||
		e.Reason != `Failed to parse query ["x]` || e.Retryable() {
		t.Errorf("err = %#v", err)
	}
}

func TestGetMissingDocumentIsNotAnError(t *testing.T) {
	c := serve(t, 404, `{"_index":"keeper-photos","_id":"x","found":false}`)
	var v map[string]any
	_, found, err := c.Get(context.Background(), PhotosIndex, "x", &v)
	if err != nil || found {
		t.Errorf("found=%v err=%v", found, err)
	}
}

func TestGetMissingIndexIsAnError(t *testing.T) {
	c := serve(t, 404, `{"error":{"type":"index_not_found_exception","reason":"no such index [keeper-photos]"},"status":404}`)
	var v map[string]any
	if _, _, err := c.Get(context.Background(), PhotosIndex, "x", &v); err == nil {
		t.Error("missing index reported as missing document")
	}
}

func TestPutConflict(t *testing.T) {
	c := serve(t, 409, `{"error":{"type":"version_conflict_engine_exception","reason":"x"}}`)
	if err := c.Put(context.Background(), PhotosIndex, "x", map[string]any{}, nil); !errors.Is(err, ErrConflict) {
		t.Errorf("err = %v, want ErrConflict", err)
	}
}

func TestEnsureIndexExisting(t *testing.T) {
	c := serve(t, 400, `{"error":{"type":"resource_already_exists_exception","reason":"exists"}}`)
	created, err := c.EnsureIndex(context.Background(), PhotosIndex, PhotosMapping)
	if err != nil || created {
		t.Errorf("created=%v err=%v", created, err)
	}
}

func TestIndexExists(t *testing.T) {
	for _, tt := range []struct {
		status int
		exists bool
		err    bool
	}{{200, true, false}, {404, false, false}, {503, false, true}} {
		exists, err := serve(t, tt.status, "").IndexExists(context.Background(), PhotosIndex)
		if exists != tt.exists || (err != nil) != tt.err {
			t.Errorf("status %d: exists=%v err=%v", tt.status, exists, err)
		}
	}
}
