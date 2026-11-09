package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/geilername/keeper/internal/events"
	"github.com/geilername/keeper/internal/photostate"
	"github.com/geilername/keeper/internal/search"
	"github.com/geilername/keeper/internal/spool"
)

// thumbSHA extracts the sha from a "/thumbs/{name}" segment of the form
// "<sha>.jpg". Anything else (other extensions, "..", slashes, upper case)
// is rejected.
func thumbSHA(name string) (string, bool) {
	sha, ok := strings.CutSuffix(name, ".jpg")
	if !ok || !photostate.ValidSHA(sha) {
		return "", false
	}
	return sha, true
}

// traceLinker builds the Dash0 deep link for a photo. The photo trace ID is
// the first 16 bytes of its sha256 (ADR-6), i.e. the first 32 hex chars.
type traceLinker struct{ tmpl string } // DASH0_TRACE_URL, contains {trace_id}

func (l traceLinker) link(sha string) (traceID, url string) {
	traceID = sha[:32]
	if l.tmpl != "" {
		url = strings.ReplaceAll(l.tmpl, "{trace_id}", traceID)
	}
	return traceID, url
}

// Snapshot is the photo shape for lists and SSE: everything the console's
// flow view draws (scores, reasons, per-stage times) without a detail fetch.
type Snapshot struct {
	SHA256      string           `json:"sha256"`
	FileName    string           `json:"file_name,omitempty"`
	Stage       Stage            `json:"stage"`
	BurstID     string           `json:"burst_id,omitempty"`
	Bucket      string           `json:"bucket,omitempty"`
	Rating      int              `json:"rating,omitempty"`
	Reasons     []string         `json:"reasons"`
	Features    *events.Features `json:"features,omitempty"`
	Attempts    int              `json:"attempts"`
	LastError   string           `json:"last_error,omitempty"`
	Captured    *time.Time       `json:"captured_at,omitempty"`
	SeenAt      time.Time        `json:"seen_at"`
	IngestedAt  *time.Time       `json:"ingested_at,omitempty"`
	ScoredAt    *time.Time       `json:"scored_at,omitempty"`
	DecidedAt   *time.Time       `json:"decided_at,omitempty"`
	PublishedAt *time.Time       `json:"published_at,omitempty"`
	FailedAt    *time.Time       `json:"failed_at,omitempty"`
	RetryAt     *time.Time       `json:"retry_at,omitempty"`
	UpdatedAt   time.Time        `json:"updated_at"`
	Thumb       string           `json:"thumb_url"`
	TraceID     string           `json:"trace_id"`
	TraceURL    string           `json:"trace_url,omitempty"`
}

func (l traceLinker) snapshot(p Photo) Snapshot {
	tid, url := l.link(p.SHA256)
	return Snapshot{
		SHA256: p.SHA256, FileName: p.FileName, Stage: p.Stage, BurstID: p.BurstID,
		Bucket: p.Bucket, Rating: p.Rating, Reasons: p.Reasons, Features: p.Features,
		Attempts: p.Attempts, LastError: p.LastError,
		Captured: p.CapturedAt, SeenAt: p.SeenAt, IngestedAt: p.IngestedAt, ScoredAt: p.ScoredAt,
		DecidedAt: p.DecidedAt, PublishedAt: p.PublishedAt, FailedAt: p.FailedAt, RetryAt: p.RetryAt,
		UpdatedAt: p.UpdatedAt,
		Thumb:     "/thumbs/" + p.SHA256 + ".jpg", TraceID: tid, TraceURL: url,
	}
}

// PhotoDetail is GET /api/photos/{sha}: the full row plus links and siblings.
type PhotoDetail struct {
	Photo
	Thumb    string     `json:"thumb_url"`
	TraceID  string     `json:"trace_id"`
	TraceURL string     `json:"trace_url,omitempty"`
	Burst    *Burst     `json:"burst,omitempty"`
	Siblings []Snapshot `json:"siblings"`
}

type api struct {
	store  *Store
	search *search.Client // nil: ELASTICSEARCH_URL not set, /api/search answers 503
	bc     *Broadcaster
	links  traceLinker
	web    fs.FS
	demo   *demoProxy     // nil unless DEMO_CONTROLS=1
	lr     *lightroomAuth // nil: Adobe Lightroom not configured
	lrMode string         // LIGHTROOM_MODE, for the status when lr is nil
}

const (
	defaultLimit = 100
	maxLimit     = 500
)

// routes builds the handler. otelhttp wraps everything except the SSE
// stream (one span per hour-long connection is noise), /healthz and the
// thumbnails (one span per image).
func (a *api) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.healthz)
	mux.HandleFunc("GET /api/photos", a.listPhotos)
	mux.HandleFunc("GET /api/photos/{sha}", a.getPhoto)
	mux.HandleFunc("GET /api/bursts/{id}", a.getBurst)
	mux.HandleFunc("GET /api/search", a.searchPhotos)
	mux.HandleFunc("GET /api/events", a.bc.ServeSSE)
	mux.HandleFunc("GET /thumbs/{name}", a.thumb)
	a.demo.register(mux)
	a.registerLightroom(mux)
	mux.Handle("GET /", http.FileServerFS(a.web))

	return otelhttp.NewHandler(mux, "gateway",
		otelhttp.WithFilter(traced),
		// After routing, name spans by pattern ("GET /api/photos/{sha}"),
		// never by raw path: span names must stay low-cardinality.
		otelhttp.WithSpanNameFormatter(func(op string, r *http.Request) string {
			if r.Pattern != "" {
				return r.Pattern
			}
			return r.Method
		}),
	)
}

// traced also leaves out the OAuth routes: the callback's query carries the
// authorization code, which must not end up in a span.
func traced(r *http.Request) bool {
	p := r.URL.Path
	return p != "/healthz" && p != "/api/events" && !strings.HasPrefix(p, "/thumbs/") &&
		!strings.HasPrefix(p, "/auth/")
}

func (a *api) healthz(w http.ResponseWriter, r *http.Request) {
	if err := a.store.Ping(r.Context()); err != nil {
		http.Error(w, "postgres: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) listPhotos(w http.ResponseWriter, r *http.Request) {
	limit := defaultLimit
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
			return
		}
		limit = min(n, maxLimit)
	}
	photos, err := a.store.ListPhotos(r.Context(), limit)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := make([]Snapshot, len(photos))
	for i, p := range photos {
		out[i] = a.links.snapshot(p)
	}
	writeJSON(w, out)
}

func (a *api) getPhoto(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	if !photostate.ValidSHA(sha) {
		http.Error(w, "sha must be 64 lowercase hex characters", http.StatusBadRequest)
		return
	}
	p, err := a.store.GetPhoto(r.Context(), sha)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	d := PhotoDetail{Photo: p, Thumb: "/thumbs/" + sha + ".jpg", Siblings: []Snapshot{}}
	d.TraceID, d.TraceURL = a.links.link(sha)
	if p.BurstID != "" {
		if b, err := a.store.GetBurst(r.Context(), p.BurstID); err == nil {
			d.Burst = &b
		} else if !errors.Is(err, ErrNotFound) {
			a.fail(w, r, err)
			return
		}
		members, err := a.store.BurstPhotos(r.Context(), p.BurstID)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		for _, m := range members {
			if m.SHA256 != sha {
				d.Siblings = append(d.Siblings, a.links.snapshot(m))
			}
		}
	}
	writeJSON(w, d)
}

func (a *api) getBurst(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, err := a.store.GetBurst(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	members, err := a.store.BurstPhotos(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	photos := make([]Snapshot, len(members))
	for i, m := range members {
		photos[i] = a.links.snapshot(m)
	}
	writeJSON(w, struct {
		Burst
		Photos []Snapshot `json:"photos"`
	}{b, photos})
}

// thumb serves /spool/thumbs/<sha>.jpg. Thumbnails are content-addressed and
// never change, so browsers may cache them forever.
func (a *api) thumb(w http.ResponseWriter, r *http.Request) {
	sha, ok := thumbSHA(r.PathValue("name"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(spool.ThumbPath(sha))
	if err != nil {
		// Not there yet (ingest still writing) or never: let the browser
		// retry later instead of caching the 404.
		w.Header().Set("Cache-Control", "no-store")
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "image/jpeg")
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("ETag", `"`+sha+`"`)
	http.ServeContent(w, r, "", st.ModTime(), f)
}

func (a *api) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	slog.ErrorContext(r.Context(), "request failed", "path", r.URL.Path, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}
