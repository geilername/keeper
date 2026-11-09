package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

func newMux(s *shooter) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /sets", s.handleSets)
	mux.HandleFunc("POST /shoot", s.handleShoot)
	return mux
}

func (s *shooter) handleSets(w http.ResponseWriter, r *http.Request) {
	sets, err := listSets(s.setsDir)
	if err != nil {
		slog.ErrorContext(r.Context(), "list sets failed", "dir", s.setsDir, "err", err)
		writeError(w, http.StatusInternalServerError, "cannot read sets directory")
		return
	}
	if sets == nil {
		sets = []setInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sets": sets})
}

func (s *shooter) handleShoot(w http.ResponseWriter, r *http.Request) {
	set := r.URL.Query().Get("set")
	if set == "" {
		writeError(w, http.StatusBadRequest, "missing ?set=<name>")
		return
	}
	dup := r.URL.Query().Get("dup") == "1"

	if !s.busy.TryLock() {
		writeError(w, http.StatusConflict, "a burst is already being uploaded")
		return
	}
	defer s.busy.Unlock()

	// A burst is a few seconds long. Finish it even if the caller hangs up,
	// so the pipeline never sees half a burst by accident.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	defer cancel()

	res, err := s.shoot(ctx, set, dup)
	switch {
	case errors.Is(err, errNoSet):
		writeError(w, http.StatusNotFound, err.Error())
	case err != nil:
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "result": res})
	default:
		writeJSON(w, http.StatusOK, res)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
