package main

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strings"
)

// traceLinker builds the Dash0 deep link for a photo from DASH0_TRACE_URL.
// The photo's trace ID is the first 16 bytes of its sha256, and the root
// span ID ("photo <file>") the next 8 (ADR-6, internal/telemetry/photo.go).
//
// Placeholders in the template:
//
//	{trace_id}     32 hex chars
//	{span_id}      16 hex chars, the photo's root span
//	{dash0_state}  Dash0's trace explorer view state for this trace, opened
//	               on the root span: https://app.dash0.com/traces/explorer?org=<org>&s={dash0_state}
type traceLinker struct {
	tmpl    string // DASH0_TRACE_URL
	dataset string // DASH0_DATASET, part of the view state
}

func (l traceLinker) link(sha string) (traceID, link string) {
	traceID = sha[:32]
	if l.tmpl == "" {
		return traceID, ""
	}
	spanID := sha[32:48]
	link = strings.NewReplacer(
		"{trace_id}", traceID,
		"{span_id}", spanID,
	).Replace(l.tmpl)
	if strings.Contains(link, "{dash0_state}") {
		state, err := dash0ViewState(l.dataset, traceID, spanID)
		if err != nil {
			return traceID, "" // no link beats a broken one
		}
		link = strings.ReplaceAll(link, "{dash0_state}", url.QueryEscape(state))
	}
	return traceID, link
}

// dash0ViewState encodes the view Dash0's trace explorer keeps in its "s"
// query parameter: JSON, zlib-compressed, base64url with padding. The shape
// mirrors a link copied from Dash0: the trace filtered by otel.trace.id, its
// waterfall open on the overview tab with the given span focused.
func dash0ViewState(dataset, traceID, spanID string) (string, error) {
	tid, err := hex.DecodeString(traceID)
	if err != nil {
		return "", err
	}
	sid, err := hex.DecodeString(spanID)
	if err != nil {
		return "", err
	}
	state := map[string]any{
		"/": map[string]any{
			"dataset":              dataset,
			"focusedTimeRange":     nil,
			"focusedDurationRange": nil,
		},
		"/traces": map[string]any{
			"spanListConfig": map[string]any{"activeViewId": "dash0-view-spans-default"},
			"id":             base64.StdEncoding.EncodeToString(sid),
			"traceId":        base64.StdEncoding.EncodeToString(tid),
			"open":           true,
			"backUrl":        nil,
			"query": map[string]any{
				"filter": []map[string]any{{"key": "otel.trace.id", "operator": "is", "value": traceID}},
			},
			"query.disabled": map[string]any{},
			"tab":            "overview",
		},
		"/traces/explorer": map[string]any{"elementKind": "span"},
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return "", err
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(buf.Bytes()), nil
}
