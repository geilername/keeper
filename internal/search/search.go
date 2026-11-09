// Package search is a small Elasticsearch client for Keeper's photo index:
// the few REST calls the indexer and the gateway need, over plain net/http.
// Every call is a CLIENT span (otelhttp) under the caller's span, so index
// writes show up in the photo's trace.
package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// PhotosIndex holds one document per photo, ID = sha256, source =
// photostate.Photo as JSON.
const PhotosIndex = "keeper-photos"

// PhotosMapping is the index definition. dynamic=false keeps fields a newer
// event contract adds in _source without indexing them, so a contract change
// never makes documents fail to index.
//
// reasons uses the english analyzer so "blurred" finds "motion blur"; the
// keyword subfields allow exact matches and aggregations.
const PhotosMapping = `{
  "settings": {"number_of_shards": 1, "number_of_replicas": 0},
  "mappings": {
    "dynamic": false,
    "properties": {
      "sha256":             {"type": "keyword"},
      "file_name":          {"type": "keyword", "fields": {"text": {"type": "text"}}},
      "captured_at":        {"type": "date"},
      "stage":              {"type": "keyword"},
      "burst_id":           {"type": "keyword"},
      "bucket":             {"type": "keyword"},
      "rating":             {"type": "integer"},
      "reasons":            {"type": "text", "analyzer": "english",
                             "fields": {"keyword": {"type": "keyword"}}},
      "attempts":           {"type": "integer"},
      "last_error":         {"type": "keyword"},
      "lightroom_asset_id": {"type": "keyword"},
      "seen_at":            {"type": "date"},
      "ingested_at":        {"type": "date"},
      "scored_at":          {"type": "date"},
      "decided_at":         {"type": "date"},
      "published_at":       {"type": "date"},
      "failed_at":          {"type": "date"},
      "retry_at":           {"type": "date"},
      "updated_at":         {"type": "date"},
      "features": {
        "properties": {
          "sharpness": {"properties": {
            "variance": {"type": "double"},
            "score":    {"type": "double"}}},
          "exposure": {"properties": {
            "mean":               {"type": "double"},
            "highlights_clipped": {"type": "double"},
            "shadows_clipped":    {"type": "double"},
            "score":              {"type": "double"}}},
          "eyes": {"properties": {
            "state":      {"type": "keyword"},
            "confidence": {"type": "double"},
            "faces":      {"type": "integer"}}},
          "dhash": {"type": "keyword"}
        }
      }
    }
  }
}`

// ErrConflict is a version conflict (HTTP 409): the document changed since
// it was read, or a create found it already there.
var ErrConflict = errors.New("elasticsearch: version conflict")

// Error is a non-2xx answer from Elasticsearch.
type Error struct {
	Op     string
	Status int
	Type   string // e.g. "mapper_parsing_exception"
	Reason string
}

func (e *Error) Error() string {
	return fmt.Sprintf("elasticsearch %s: %d %s: %s", e.Op, e.Status, e.Type, e.Reason)
}

// Retryable reports whether trying again later can succeed: overload and
// server errors yes, a request Elasticsearch rejected as invalid no.
func (e *Error) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// Version is a document's optimistic-concurrency token.
type Version struct {
	SeqNo       int64 `json:"_seq_no"`
	PrimaryTerm int64 `json:"_primary_term"`
}

// Client is safe for concurrent use.
type Client struct {
	baseURL string
	timeout time.Duration
	client  *http.Client
}

// New returns a client for baseURL (e.g. http://elasticsearch:9200).
// timeout bounds each call, including reading the response.
func New(baseURL string, timeout time.Duration) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		timeout: timeout,
		client: &http.Client{
			Transport: otelhttp.NewTransport(http.DefaultTransport,
				// Semconv client span name: "{method} {url.template}".
				otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
					if t, ok := r.Context().Value(templateKey{}).(string); ok {
						return r.Method + " " + t
					}
					return r.Method
				})),
		},
	}
}

type templateKey struct{}

// Ping checks that the cluster answers.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.do(ctx, "ping", http.MethodGet, "/", "/", nil, nil)
	return err
}

// EnsureIndex creates index with body unless it exists. created is true
// only when this call created it.
func (c *Client) EnsureIndex(ctx context.Context, index, body string) (created bool, err error) {
	_, err = c.do(ctx, "create_index", http.MethodPut, "/{index}", "/"+url.PathEscape(index),
		[]byte(body), nil)
	var e *Error
	if errors.As(err, &e) && e.Type == "resource_already_exists_exception" {
		return false, nil
	}
	return err == nil, err
}

// Get reads a document's source into v. found is false (and v untouched)
// when the document does not exist.
func (c *Client) Get(ctx context.Context, index, id string, v any) (ver Version, found bool, err error) {
	var res struct {
		Version
		Found  bool            `json:"found"`
		Source json.RawMessage `json:"_source"`
	}
	status, err := c.do(ctx, "get", http.MethodGet, "/{index}/_doc/{id}", docPath(index, id), nil, &res)
	if status == http.StatusNotFound && res.Source == nil {
		var e *Error
		// 404 with found=false is a missing document; 404 with an error
		// type (index_not_found_exception) is a real error.
		if !errors.As(err, &e) || e.Type == "" {
			return Version{}, false, nil
		}
	}
	if err != nil {
		return Version{}, false, err
	}
	if !res.Found {
		return Version{}, false, nil
	}
	return res.Version, true, json.Unmarshal(res.Source, v)
}

// Put writes v as the document's source. With ver nil it only creates the
// document; otherwise it only replaces the version that was read. Either
// way a concurrent change makes it return ErrConflict.
func (c *Client) Put(ctx context.Context, index, id string, v any, ver *Version) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	q := url.Values{}
	if ver == nil {
		q.Set("op_type", "create")
	} else {
		q.Set("if_seq_no", strconv.FormatInt(ver.SeqNo, 10))
		q.Set("if_primary_term", strconv.FormatInt(ver.PrimaryTerm, 10))
	}
	status, err := c.do(ctx, "index", http.MethodPut, "/{index}/_doc/{id}",
		docPath(index, id)+"?"+q.Encode(), data, nil)
	if status == http.StatusConflict {
		return ErrConflict
	}
	return err
}

// SearchResult is the part of a _search answer callers use.
type SearchResult struct {
	Total int               `json:"total"`
	Hits  []json.RawMessage `json:"hits"` // each hit's _source
}

// Search runs a _search request body against index.
func (c *Client) Search(ctx context.Context, index string, body any) (SearchResult, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return SearchResult{}, err
	}
	var res struct {
		Hits struct {
			Total struct {
				Value int `json:"value"`
			} `json:"total"`
			Hits []struct {
				Source json.RawMessage `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if _, err := c.do(ctx, "search", http.MethodPost, "/{index}/_search",
		"/"+url.PathEscape(index)+"/_search", data, &res); err != nil {
		return SearchResult{}, err
	}
	out := SearchResult{Total: res.Hits.Total.Value, Hits: make([]json.RawMessage, len(res.Hits.Hits))}
	for i, h := range res.Hits.Hits {
		out.Hits[i] = h.Source
	}
	return out, nil
}

func docPath(index, id string) string {
	return "/" + url.PathEscape(index) + "/_doc/" + url.PathEscape(id)
}

// do sends one request with the per-call timeout. A 2xx JSON body is
// decoded into out (if non-nil); a 404 body too, since a missing document
// is an answer, not only an error. Non-2xx answers become *Error.
func (c *Client) do(ctx context.Context, op, method, template, path string, body []byte, out any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	ctx = context.WithValue(ctx, templateKey{}, template)

	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rd)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("elasticsearch %s: %w", op, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("elasticsearch %s: read body: %w", op, err)
	}

	ok := resp.StatusCode < 300
	if (ok || resp.StatusCode == http.StatusNotFound) && out != nil {
		// A 404 error body has no fields of out, so this cannot fail it.
		if err := json.Unmarshal(data, out); err != nil && ok {
			return resp.StatusCode, fmt.Errorf("elasticsearch %s: decode: %w", op, err)
		}
	}
	if ok {
		return resp.StatusCode, nil
	}
	e := &Error{Op: op, Status: resp.StatusCode}
	type cause struct {
		Type   string `json:"type"`
		Reason string `json:"reason"`
	}
	var eb struct {
		Error struct {
			cause
			RootCause []cause `json:"root_cause"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &eb) == nil {
		e.Type, e.Reason = eb.Error.Type, eb.Error.Reason
		// A failed search says only "all shards failed"; the root cause
		// says what is wrong ("Failed to parse query [...]").
		if len(eb.Error.RootCause) > 0 && eb.Error.RootCause[0].Reason != "" {
			e.Reason = eb.Error.RootCause[0].Reason
		}
	}
	if e.Reason == "" {
		e.Reason = strings.TrimSpace(string(data[:min(len(data), 300)]))
	}
	return resp.StatusCode, e
}
