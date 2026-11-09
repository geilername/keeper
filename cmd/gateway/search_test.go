package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/geilername/keeper/internal/search"
)

func TestSearchQuery(t *testing.T) {
	q := searchQuery("", "", "", "", 10)
	b, _ := json.Marshal(q)
	if !strings.Contains(string(b), `"match_all"`) || strings.Contains(string(b), `"_score"`) {
		t.Errorf("no q: want match_all sorted by seen_at, got %s", b)
	}

	q = searchQuery("eyes closed", "reject", "published", "b1", 10)
	b, _ = json.Marshal(q)
	for _, want := range []string{`"query_string"`, `"query":"eyes closed"`, `"_score"`,
		`{"term":{"bucket":"reject"}}`, `{"term":{"stage":"published"}}`, `{"term":{"burst_id":"b1"}}`,
		`"allow_leading_wildcard":false`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("query lacks %s: %s", want, b)
		}
	}
}

// fakeSearch answers _search like Elasticsearch: status and body as given.
func fakeSearch(t *testing.T, status int, body string, gotBody *string) *search.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+search.PhotosIndex+"/_search" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if gotBody != nil {
			b, _ := io.ReadAll(r.Body)
			*gotBody = string(b)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return search.New(srv.URL, time.Second)
}

func TestSearchEndpoint(t *testing.T) {
	hit := `{"sha256":"` + testSHA + `","stage":"published","bucket":"reject","rating":1,` +
		`"reasons":["eyes closed"],"attempts":1,"seen_at":"2026-11-09T12:00:00Z","updated_at":"2026-11-09T12:00:01Z"}`
	var sent string
	a := &api{search: fakeSearch(t, 200,
		`{"hits":{"total":{"value":7},"hits":[{"_source":`+hit+`}]}}`, &sent),
		links: traceLinker{tmpl: "https://dash0/{trace_id}"}}

	rec := httptest.NewRecorder()
	a.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/search?q=eyes+closed&bucket=reject&limit=5", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var res SearchResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Total != 7 || len(res.Photos) != 1 {
		t.Fatalf("result = %+v", res)
	}
	p := res.Photos[0]
	if p.SHA256 != testSHA || p.Stage != "published" || p.Thumb != "/thumbs/"+testSHA+".webp" ||
		p.TraceURL != "https://dash0/"+testSHA[:32] {
		t.Errorf("snapshot = %+v", p)
	}
	if !strings.Contains(sent, `"size":5`) || !strings.Contains(sent, `"query":"eyes closed"`) {
		t.Errorf("request body = %s", sent)
	}
}

func TestSearchErrors(t *testing.T) {
	tests := []struct {
		name   string
		api    *api
		url    string
		status int
	}{
		{"not configured", &api{}, "/api/search?q=x", http.StatusServiceUnavailable},
		{"bad limit", &api{search: fakeSearch(t, 200, `{}`, nil)}, "/api/search?limit=0", http.StatusBadRequest},
		{"bad query", &api{search: fakeSearch(t, 400,
			`{"error":{"type":"search_phase_execution_exception","reason":"Failed to parse query"}}`, nil)},
			"/api/search?q=%22unbalanced", http.StatusBadRequest},
		{"no index yet", &api{search: fakeSearch(t, 404,
			`{"error":{"type":"index_not_found_exception","reason":"no such index"}}`, nil)},
			"/api/search", http.StatusOK},
		{"es down", &api{search: fakeSearch(t, 503, `{"error":{"type":"x","reason":"y"}}`, nil)},
			"/api/search", http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tt.api.routes().ServeHTTP(rec, httptest.NewRequest("GET", tt.url, nil))
			if rec.Code != tt.status {
				t.Errorf("status = %d, want %d: %s", rec.Code, tt.status, rec.Body)
			}
		})
	}
}
