package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const (
	maxMasterBytes = 200 << 20 // Lightroom's per-call master upload limit
	maxJSONBytes   = 1 << 20
)

type server struct {
	catalogID string
	store     *store
	faults    *faults
	requests  metric.Int64Counter
}

func newServer(catalogID string, meter metric.Meter) *server {
	// Creating a counter only fails for an invalid name, which is a constant.
	requests, _ := meter.Int64Counter("keeper.lightroom.requests",
		metric.WithDescription("Lightroom API requests seen by the mock, by route and result"),
		metric.WithUnit("{request}"))
	return &server{catalogID: catalogID, store: newStore(), faults: newFaults(), requests: requests}
}

// handler returns the routes without OTel wrapping; main adds otelhttp.
func (s *server) handler() http.Handler {
	mux := http.NewServeMux()

	// Never subject to faults.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /admin/faults", s.getFaults)
	mux.HandleFunc("POST /admin/faults", s.postFaults)
	mux.HandleFunc("POST /admin/reset", s.postReset)
	mux.HandleFunc("GET /admin/stats", s.getStats)

	// Lightroom-shaped API (paths as in lr.adobe.io/v2). Faults apply here.
	const asset = "/v2/catalogs/{catalog_id}/assets/{asset_id}"
	s.api(mux, "GET /v2/catalog", "get_catalog", s.getCatalog)
	s.api(mux, "GET /v2/catalogs/{catalog_id}/assets", "list_assets", s.listAssets)
	s.api(mux, "PUT "+asset, "create_asset", s.createAsset)
	s.api(mux, "GET "+asset, "get_asset", s.getAsset)
	s.api(mux, "PATCH "+asset, "set_rating", s.setRating)
	s.api(mux, "PUT "+asset+"/master", "upload_master", s.uploadMaster)
	return mux
}

// response is what an API handler returns; the api wrapper writes it and
// records the metric.
type response struct {
	status int
	body   any
	result string // metric label; empty means derive it from status
}

func (r response) resultLabel() string {
	switch {
	case r.result != "":
		return r.result
	case r.status < 400:
		return "ok"
	case r.status < 500:
		return "client_error"
	default:
		return "server_error"
	}
}

// apiError has the shape of Lightroom's error bodies.
type apiError struct {
	Code        int    `json:"code"`
	Description string `json:"description"`
}

func errResp(status int, format string, args ...any) response {
	return response{status: status, body: apiError{Code: status, Description: fmt.Sprintf(format, args...)}}
}

// api registers a Lightroom route behind the fault injector. Order per
// request: injected latency, injected failure, path validation, handler.
func (s *server) api(mux *http.ServeMux, pattern, route string, h func(*http.Request) response) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		span := trace.SpanFromContext(ctx)
		record := func(result string) {
			s.requests.Add(ctx, 1, metric.WithAttributes(
				attribute.String("route", route), attribute.String("result", result)))
		}

		latency, failStatus := s.faults.decide()
		if latency > 0 {
			span.SetAttributes(attribute.Int64("fault.latency_ms", latency.Milliseconds()))
			select {
			case <-time.After(latency):
			case <-ctx.Done():
				record("canceled") // client gave up while we were "slow"
				return
			}
		}

		var resp response
		if failStatus != 0 {
			span.SetAttributes(attribute.Bool("fault.injected", true))
			slog.WarnContext(ctx, "fault injected", "route", route, "status", failStatus)
			resp = errResp(failStatus, "injected fault")
			resp.result = "fault"
		} else if id := r.PathValue("catalog_id"); id != "" && id != s.catalogID {
			resp = errResp(http.StatusNotFound, "catalog %q not found", id)
		} else if id := r.PathValue("asset_id"); id != "" && !isLowerHex(id, 32) {
			resp = errResp(http.StatusBadRequest, "asset_id must be 32 lowercase hex characters")
		} else {
			r.Body = http.MaxBytesReader(w, r.Body, maxMasterBytes)
			resp = h(r)
		}
		writeJSON(w, resp.status, resp.body)
		record(resp.resultLabel())
	})
}

func (s *server) getCatalog(*http.Request) response {
	return response{status: http.StatusOK, body: map[string]any{
		"id": s.catalogID, "type": "catalog", "subtype": "lightroom",
		"payload": map[string]string{"name": "Keeper mock catalog"},
	}}
}

func (s *server) listAssets(*http.Request) response {
	assets := s.store.list()
	return response{status: http.StatusOK, body: map[string]any{"resources": assets, "count": len(assets)}}
}

func (s *server) getAsset(r *http.Request) response {
	a, err := s.store.get(r.PathValue("asset_id"))
	if err != nil {
		return errResp(http.StatusNotFound, "%v", err)
	}
	return response{status: http.StatusOK, body: a}
}

// createAsset: PUT with a client-chosen ID. 201 when new, 200 with the
// existing asset when the ID is already known (idempotent replay).
func (s *server) createAsset(r *http.Request) response {
	var req struct {
		Subtype string  `json:"subtype"`
		Payload Payload `json:"payload"`
	}
	// An empty body is fine (curl in the demo); Lightroom clients send one.
	if err := decodeJSON(r, &req, false); err != nil && !errors.Is(err, io.EOF) {
		return errResp(http.StatusBadRequest, "invalid body: %v", err)
	}
	switch req.Subtype {
	case "":
		req.Subtype = "image"
	case "image", "video":
	default:
		return errResp(http.StatusBadRequest, "subtype must be image or video")
	}
	if sha := req.Payload.ImportSource.SHA256; sha != "" && !isLowerHex(sha, 64) {
		return errResp(http.StatusBadRequest, "importSource.sha256 must be 64 lowercase hex characters")
	}

	a, created, err := s.store.create(r.PathValue("asset_id"), req.Subtype, req.Payload)
	switch {
	case errors.Is(err, errConflict):
		return errResp(http.StatusConflict, "%v", err)
	case errors.Is(err, errSHAInUse):
		return errResp(http.StatusPreconditionFailed, "%v: %s", err, a.ID)
	case err != nil:
		return errResp(http.StatusInternalServerError, "%v", err)
	case created:
		return response{status: http.StatusCreated, body: a}
	default:
		return response{status: http.StatusOK, body: a, result: "existing"}
	}
}

func (s *server) setRating(r *http.Request) response {
	var req struct {
		Rating int `json:"rating"`
	}
	if err := decodeJSON(r, &req, true); err != nil {
		return errResp(http.StatusBadRequest, "invalid body: %v", err)
	}
	if req.Rating < 1 || req.Rating > 5 {
		return errResp(http.StatusBadRequest, "rating must be between 1 and 5")
	}
	a, err := s.store.setRating(r.PathValue("asset_id"), req.Rating)
	if err != nil {
		return errResp(http.StatusNotFound, "%v", err)
	}
	return response{status: http.StatusOK, body: a}
}

// uploadMaster stores only size and sha256 of the original, not the bytes.
func (s *server) uploadMaster(r *http.Request) response {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if !strings.HasPrefix(mt, "image/") && !strings.HasPrefix(mt, "video/") && mt != "application/octet-stream" {
		return errResp(http.StatusUnsupportedMediaType, "content type must be image/*, video/* or application/octet-stream")
	}
	id := r.PathValue("asset_id")
	if _, err := s.store.get(id); err != nil {
		return errResp(http.StatusNotFound, "%v", err)
	}
	h := sha256.New()
	n, err := io.Copy(h, r.Body)
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		return errResp(http.StatusRequestEntityTooLarge, "master larger than %d bytes", maxMasterBytes)
	case err != nil:
		return errResp(http.StatusBadRequest, "reading body: %v", err)
	case n == 0:
		return errResp(http.StatusBadRequest, "empty body")
	}

	a, created, err := s.store.setMaster(id, n, hex.EncodeToString(h.Sum(nil)))
	switch {
	case errors.Is(err, errNotFound):
		return errResp(http.StatusNotFound, "%v", err)
	case errors.Is(err, errMasterExists):
		return errResp(http.StatusConflict, "%v", err)
	case err != nil:
		return errResp(http.StatusInternalServerError, "%v", err)
	case created:
		return response{status: http.StatusCreated, body: a}
	default:
		return response{status: http.StatusOK, body: a, result: "existing"}
	}
}

func (s *server) getFaults(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.faults.get())
}

// postFaults replaces the whole fault config; unknown fields are rejected so
// a typo on stage fails loudly instead of silently doing nothing.
func (s *server) postFaults(w http.ResponseWriter, r *http.Request) {
	var cfg faultConfig
	if err := decodeJSON(r, &cfg, true); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Code: 400, Description: "invalid body: " + err.Error()})
		return
	}
	if err := cfg.normalize(); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Code: 400, Description: err.Error()})
		return
	}
	s.faults.set(cfg)
	slog.InfoContext(r.Context(), "faults updated",
		"error_rate", cfg.ErrorRate, "status", cfg.Status, "latency_ms", cfg.LatencyMS)
	writeJSON(w, http.StatusOK, cfg)
}

// postReset heals all faults and empties the catalogue.
func (s *server) postReset(w http.ResponseWriter, r *http.Request) {
	s.faults.reset()
	s.store.reset()
	slog.InfoContext(r.Context(), "mock reset: faults cleared, catalogue emptied")
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) getStats(w http.ResponseWriter, _ *http.Request) {
	st := s.store.stats()
	st.FaultsInjected = s.faults.injectedCount()
	st.Faults = s.faults.get()
	writeJSON(w, http.StatusOK, st)
}

func decodeJSON(r *http.Request, v any, strict bool) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxJSONBytes))
	if strict {
		dec.DisallowUnknownFields()
	}
	return dec.Decode(v)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	if body == nil {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// isLowerHex keeps IDs canonical: "ABC…" and "abc…" must not become two assets.
func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}
