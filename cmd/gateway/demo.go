package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Demo controls: the console's fault panel talks to lightroom-mock's admin
// API through the gateway, so the browser needs only one origin. The routes
// exist only when DEMO_CONTROLS=1; otherwise they answer 404 and the console
// keeps the panel hidden. Only two upstream paths are reachable, and only a
// fault config that passes validation is forwarded.
//
//	GET  /api/demo/faults → GET  {LIGHTROOM_ADMIN_URL}/admin/faults
//	POST /api/demo/faults → POST {LIGHTROOM_ADMIN_URL}/admin/faults
//	GET  /api/demo/stats  → GET  {LIGHTROOM_ADMIN_URL}/admin/stats

const (
	demoMaxBody     = 4 << 10  // a fault config is ~60 bytes
	demoMaxUpstream = 64 << 10 // stats and faults are small JSON objects
	demoMaxLatency  = 120_000  // lightroom-mock's own limit, in ms
)

// demoFaults mirrors lightroom-mock's faultConfig. Pointers tell "missing"
// from zero: the mock replaces its whole config on POST, so the console must
// send every field and a forgotten one is an error, not a silent heal.
type demoFaults struct {
	ErrorRate *float64 `json:"error_rate"`
	Status    *int     `json:"status"`
	LatencyMS *int     `json:"latency_ms"`
}

func (f demoFaults) validate() error {
	switch {
	case f.ErrorRate == nil || f.Status == nil || f.LatencyMS == nil:
		return errors.New("error_rate, status and latency_ms are all required")
	case *f.ErrorRate < 0 || *f.ErrorRate > 1:
		return errors.New("error_rate must be between 0 and 1")
	case *f.Status < 400 || *f.Status > 599:
		return errors.New("status must be between 400 and 599")
	case *f.LatencyMS < 0 || *f.LatencyMS > demoMaxLatency:
		return errors.New("latency_ms must be between 0 and 120000")
	}
	return nil
}

// decodeDemoFaults reads exactly one JSON object with the three known fields.
func decodeDemoFaults(r io.Reader) (demoFaults, error) {
	var f demoFaults
	dec := json.NewDecoder(io.LimitReader(r, demoMaxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return f, err
	}
	if dec.More() {
		return f, errors.New("trailing data after the JSON object")
	}
	return f, f.validate()
}

type demoProxy struct {
	base   string // LIGHTROOM_ADMIN_URL without a trailing slash
	client *http.Client
}

func newDemoProxy(base string) *demoProxy {
	return &demoProxy{
		base: strings.TrimRight(base, "/"),
		client: &http.Client{
			Timeout:   3 * time.Second,
			Transport: otelhttp.NewTransport(http.DefaultTransport),
		},
	}
}

// register adds the demo routes. GET and POST on any other /api/demo/ path,
// and on every one while DEMO_CONTROLS is off (p == nil), are a plain 404:
// the console then hides the panel, and these paths never fall through to
// the static file server. (A method-less "/api/demo/" pattern would conflict
// with "GET /" in ServeMux.)
func (p *demoProxy) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/demo/", http.NotFound)
	mux.HandleFunc("POST /api/demo/", http.NotFound)
	if p == nil {
		return
	}
	mux.HandleFunc("GET /api/demo/faults", p.forwardGet("/admin/faults"))
	mux.HandleFunc("GET /api/demo/stats", p.forwardGet("/admin/stats"))
	mux.HandleFunc("POST /api/demo/faults", p.postFaults)
}

func (p *demoProxy) forwardGet(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p.forward(w, r, http.MethodGet, path, nil)
	}
}

func (p *demoProxy) postFaults(w http.ResponseWriter, r *http.Request) {
	// Requiring application/json means a cross-site form cannot post here:
	// a browser sends this content type cross-origin only after a CORS
	// preflight, which the gateway never grants.
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	f, err := decodeDemoFaults(r.Body)
	if err != nil {
		http.Error(w, "invalid fault config: "+err.Error(), http.StatusBadRequest)
		return
	}
	body, _ := json.Marshal(f) // re-encoded: only the validated fields go upstream
	p.forward(w, r, http.MethodPost, "/admin/faults", body)
}

// forward calls the upstream and relays its status and JSON body. A body that
// is not JSON (a proxy error page, a wrong URL) becomes a 502 instead.
func (p *demoProxy) forward(w http.ResponseWriter, r *http.Request, method, path string, body []byte) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, p.base+path, bytes.NewReader(body))
	if err != nil {
		p.badGateway(w, r, err)
		return
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := p.client.Do(req)
	if err != nil {
		p.badGateway(w, r, err)
		return
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, demoMaxUpstream+1))
	switch {
	case err != nil:
		p.badGateway(w, r, err)
		return
	case len(data) > demoMaxUpstream:
		p.badGateway(w, r, errors.New("upstream response too large"))
		return
	case !json.Valid(data):
		p.badGateway(w, r, errors.New("upstream answered "+res.Status+" without JSON"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(res.StatusCode)
	_, _ = w.Write(data)
}

func (p *demoProxy) badGateway(w http.ResponseWriter, r *http.Request, err error) {
	slog.WarnContext(r.Context(), "demo proxy: lightroom-mock unreachable", "path", r.URL.Path, "err", err)
	http.Error(w, "lightroom-mock admin API unreachable", http.StatusBadGateway)
}
