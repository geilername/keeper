package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/geilername/keeper/internal/search"
)

// searchFields are what a query term without a field name matches.
var searchFields = []string{"reasons", "file_name.text"}

// SearchResult is GET /api/search.
type SearchResult struct {
	Total  int        `json:"total"`
	Photos []Snapshot `json:"photos"`
}

// searchQuery builds the Elasticsearch request for GET /api/search.
//
// q uses the query_string syntax, so besides words that match the reasons
// ("blur", "eyes closed") it takes fields and ranges:
// "features.eyes.state:closed AND features.sharpness.score:>0.5",
// "attempts:>2", "stage:retrying". bucket, stage and burst are exact
// filters. Without q the newest photos come first, with q the best matches.
func searchQuery(q, bucket, stage, burst string, limit int) map[string]any {
	var must any = map[string]any{"match_all": map[string]any{}}
	sort := []any{map[string]any{"seen_at": "desc"}, map[string]any{"sha256": "asc"}}
	if q != "" {
		must = map[string]any{"query_string": map[string]any{
			"query":                  q,
			"fields":                 searchFields,
			"default_operator":       "AND",
			"allow_leading_wildcard": false,
		}}
		sort = append([]any{"_score"}, sort...)
	}
	filter := []any{}
	for _, t := range [][2]string{{"bucket", bucket}, {"stage", stage}, {"burst_id", burst}} {
		if t[1] != "" {
			filter = append(filter, map[string]any{"term": map[string]any{t[0]: t[1]}})
		}
	}
	return map[string]any{
		"size":             limit,
		"track_total_hits": true,
		"query":            map[string]any{"bool": map[string]any{"must": must, "filter": filter}},
		"sort":             sort,
	}
}

func (a *api) searchPhotos(w http.ResponseWriter, r *http.Request) {
	if a.search == nil {
		http.Error(w, "search is not configured (ELASTICSEARCH_URL)", http.StatusServiceUnavailable)
		return
	}
	qs := r.URL.Query()
	limit := defaultLimit
	if s := qs.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
			return
		}
		limit = min(n, maxLimit)
	}
	body := searchQuery(strings.TrimSpace(qs.Get("q")), qs.Get("bucket"), qs.Get("stage"),
		qs.Get("burst"), limit)

	res, err := a.search.Search(r.Context(), search.PhotosIndex, body)
	var se *search.Error
	switch {
	case errors.As(err, &se) && se.Status == http.StatusBadRequest:
		// The query itself is wrong (e.g. unbalanced quotes): tell the user.
		http.Error(w, "bad query: "+se.Reason, http.StatusBadRequest)
		return
	case errors.As(err, &se) && se.Type == "index_not_found_exception":
		writeJSON(w, SearchResult{Photos: []Snapshot{}}) // indexer has not started yet
		return
	case err != nil:
		slog.ErrorContext(r.Context(), "search failed", "err", err)
		http.Error(w, "search unavailable", http.StatusServiceUnavailable)
		return
	}

	out := SearchResult{Total: res.Total, Photos: make([]Snapshot, 0, len(res.Hits))}
	for _, h := range res.Hits {
		var p Photo
		if err := json.Unmarshal(h, &p); err != nil {
			slog.WarnContext(r.Context(), "skipping undecodable search hit", "err", err)
			continue
		}
		if p.Reasons == nil {
			p.Reasons = []string{}
		}
		out.Photos = append(out.Photos, a.links.snapshot(p))
	}
	writeJSON(w, out)
}
