package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

const (
	cat    = defaultCatalogID
	idA    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	idB    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	shaA   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	assets = "/v2/catalogs/" + cat + "/assets"
)

func assetPath(id string) string { return assets + "/" + id }

func createBody(sha string) string {
	return `{"subtype":"image","payload":{"captureDate":"2026-11-09T10:00:00","importSource":{"fileName":"DSC_0001.JPG","sha256":"` + sha + `"}}}`
}

type req struct {
	method, path, body, contentType string
}

func newTestServer() *server { return newServer(cat, noop.NewMeterProvider().Meter("test")) }

func do(t *testing.T, h http.Handler, r req) *httptest.ResponseRecorder {
	t.Helper()
	hr := httptest.NewRequest(r.method, r.path, strings.NewReader(r.body))
	ct := r.contentType
	if ct == "" && r.body != "" {
		ct = "application/json"
	}
	if ct != "" {
		hr.Header.Set("Content-Type", ct)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, hr)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return v
}

// step is one request in a scenario and the status it must get.
type step struct {
	name string
	req
	want int
}

func runSteps(t *testing.T, h http.Handler, steps []step) {
	t.Helper()
	for _, st := range steps {
		if rec := do(t, h, st.req); rec.Code != st.want {
			t.Fatalf("%s: %s %s = %d, want %d (body %s)", st.name, st.method, st.path, rec.Code, st.want, rec.Body)
		}
	}
}

func TestCreateAssetIsIdempotent(t *testing.T) {
	s := newTestServer()
	h := s.handler()

	runSteps(t, h, []step{
		{"first create", req{"PUT", assetPath(idA), createBody(shaA), ""}, 201},
		{"same id again", req{"PUT", assetPath(idA), createBody(shaA), ""}, 200},
		{"same id, no body", req{"PUT", assetPath(idA), "", ""}, 200},
		{"same id, other sha", req{"PUT", assetPath(idA), createBody(shaB), ""}, 409},
		{"other id, same sha", req{"PUT", assetPath(idB), createBody(shaA), ""}, 412},
		{"uppercase id", req{"PUT", assetPath(strings.ToUpper(idB)), createBody(shaB), ""}, 400},
		{"short id", req{"PUT", assetPath("abc"), createBody(shaB), ""}, 400},
		{"bad sha", req{"PUT", assetPath(idB), createBody("xyz"), ""}, 400},
		{"bad subtype", req{"PUT", assetPath(idB), `{"subtype":"pdf"}`, ""}, 400},
		{"wrong catalog", req{"PUT", "/v2/catalogs/ffffffffffffffffffffffffffffffff/assets/" + idB, createBody(shaB), ""}, 404},
		{"get", req{"GET", assetPath(idA), "", ""}, 200},
		{"get unknown", req{"GET", assetPath(idB), "", ""}, 404},
	})

	list := decode[struct {
		Count     int     `json:"count"`
		Resources []Asset `json:"resources"`
	}](t, do(t, h, req{"GET", assets, "", ""}))
	if list.Count != 1 || len(list.Resources) != 1 {
		t.Fatalf("list count = %d (%d resources), want exactly 1 asset", list.Count, len(list.Resources))
	}
	a := list.Resources[0]
	if a.ID != idA || a.Payload.ImportSource.SHA256 != shaA || a.Created.IsZero() {
		t.Errorf("listed asset = %+v", a)
	}
	if st := s.store.stats(); st.CreateReplays != 2 {
		t.Errorf("create_replays = %d, want 2", st.CreateReplays)
	}
}

func TestSetRating(t *testing.T) {
	tests := []struct {
		name   string
		id     string
		body   string
		want   int
		rating int // expected stored rating when want == 200
	}{
		{"five stars", idA, `{"rating":5}`, 200, 5},
		{"one star", idA, `{"rating":1}`, 200, 1},
		{"zero", idA, `{"rating":0}`, 400, 0},
		{"six", idA, `{"rating":6}`, 400, 0},
		{"unknown field", idA, `{"stars":3}`, 400, 0},
		{"not json", idA, `five`, 400, 0},
		{"unknown asset", idB, `{"rating":3}`, 404, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestServer().handler()
			runSteps(t, h, []step{{"create", req{"PUT", assetPath(idA), createBody(shaA), ""}, 201}})

			rec := do(t, h, req{"PATCH", assetPath(tt.id), tt.body, ""})
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.want, rec.Body)
			}
			if tt.want != 200 {
				return
			}
			got := decode[Asset](t, do(t, h, req{"GET", assetPath(tt.id), "", ""}))
			if r := got.Payload.Ratings[ratingApp].Rating; r != tt.rating {
				t.Errorf("stored rating = %d, want %d", r, tt.rating)
			}
		})
	}
}

func TestUploadMaster(t *testing.T) {
	h := newTestServer().handler()
	master := assetPath(idA) + "/master"
	runSteps(t, h, []step{
		{"unknown asset", req{"PUT", assetPath(idB) + "/master", "jpegbytes", "image/jpeg"}, 404},
		{"create", req{"PUT", assetPath(idA), createBody(shaA), ""}, 201},
		{"wrong content type", req{"PUT", master, "jpegbytes", "text/plain"}, 415},
		{"empty", req{"PUT", master, "", "image/jpeg"}, 400},
		{"upload", req{"PUT", master, "jpegbytes", "image/jpeg"}, 201},
		{"same bytes again", req{"PUT", master, "jpegbytes", "application/octet-stream"}, 200},
		{"different bytes", req{"PUT", master, "otherbytes", "image/jpeg"}, 409},
	})
	got := decode[Asset](t, do(t, h, req{"GET", assetPath(idA), "", ""}))
	if m := got.Payload.Master; m == nil || m.SizeBytes != int64(len("jpegbytes")) {
		t.Errorf("master = %+v, want %d bytes", m, len("jpegbytes"))
	}
}

func TestFaultRate(t *testing.T) {
	tests := []struct {
		name       string
		faults     string
		wantStatus int // status every API request must get
	}{
		{"rate 1 fails all with 503", `{"error_rate":1}`, 503},
		{"rate 1 with custom status", `{"error_rate":1,"status":500}`, 500},
		{"rate 0 fails none", `{"error_rate":0}`, 200},
		{"empty config heals", `{}`, 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer()
			h := s.handler()
			runSteps(t, h, []step{{"create", req{"PUT", assetPath(idA), createBody(shaA), ""}, 201}})
			runSteps(t, h, []step{{"set faults", req{"POST", "/admin/faults", tt.faults, ""}, 200}})

			const n = 200
			for i := range n {
				for _, r := range []req{
					{"GET", "/v2/catalog", "", ""},
					{"GET", assets, "", ""},
					{"PUT", assetPath(idA), createBody(shaA), ""}, // replay: 200
					{"PATCH", assetPath(idA), `{"rating":3}`, ""},
				} {
					if rec := do(t, h, r); rec.Code != tt.wantStatus {
						t.Fatalf("request %d %s %s = %d, want %d", i, r.method, r.path, rec.Code, tt.wantStatus)
					}
				}
			}
			wantInjected := 0
			if tt.wantStatus != 200 {
				wantInjected = 4 * n
			}
			if got := s.faults.injectedCount(); got != wantInjected {
				t.Errorf("faults injected = %d, want %d", got, wantInjected)
			}
		})
	}
}

func TestAdminAndHealthUnaffectedByFaults(t *testing.T) {
	s := newTestServer()
	h := s.handler()
	runSteps(t, h, []step{
		{"break everything", req{"POST", "/admin/faults", `{"error_rate":1,"latency_ms":0}`, ""}, 200},
		{"api is broken", req{"GET", "/v2/catalog", "", ""}, 503},
		{"healthz", req{"GET", "/healthz", "", ""}, 204},
		{"get faults", req{"GET", "/admin/faults", "", ""}, 200},
		{"stats", req{"GET", "/admin/stats", "", ""}, 200},
		{"set faults again", req{"POST", "/admin/faults", `{"error_rate":1}`, ""}, 200},
		{"reset", req{"POST", "/admin/reset", "", ""}, 204},
		{"api healed by reset", req{"GET", "/v2/catalog", "", ""}, 200},
	})
}

func TestPostFaultsValidation(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{"valid", `{"error_rate":0.5,"status":502,"latency_ms":1500}`, 200},
		{"rate above 1", `{"error_rate":1.5}`, 400},
		{"negative rate", `{"error_rate":-0.1}`, 400},
		{"2xx status", `{"error_rate":1,"status":200}`, 400},
		{"negative latency", `{"latency_ms":-1}`, 400},
		{"typo in field", `{"errorrate":1}`, 400},
		{"not json", `break it`, 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestServer().handler()
			if rec := do(t, h, req{"POST", "/admin/faults", tt.body, ""}); rec.Code != tt.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.want, rec.Body)
			}
		})
	}
}

func TestLatencyInjection(t *testing.T) {
	h := newTestServer().handler()
	runSteps(t, h, []step{{"slow down", req{"POST", "/admin/faults", `{"latency_ms":50}`, ""}, 200}})

	start := time.Now()
	runSteps(t, h, []step{{"slow but ok", req{"GET", "/v2/catalog", "", ""}, 200}})
	if d := time.Since(start); d < 50*time.Millisecond {
		t.Errorf("request took %v, want >= 50ms", d)
	}

	start = time.Now()
	runSteps(t, h, []step{{"admin stays fast", req{"GET", "/admin/stats", "", ""}, 200}})
	if d := time.Since(start); d >= 50*time.Millisecond {
		t.Errorf("admin request took %v, want no injected latency", d)
	}

	// A client that gives up mid-latency gets nothing written.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, "GET", "/v2/catalog", nil))
	if rec.Body.Len() != 0 {
		t.Errorf("canceled request got body %q", rec.Body)
	}
}

func TestFaultTelemetry(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	s := newServer(cat, mp.Meter("test"))
	h := otelhttp.NewHandler(s.handler(), "test", otelhttp.WithTracerProvider(tp))

	runSteps(t, h, []step{
		{"create", req{"PUT", assetPath(idA), createBody(shaA), ""}, 201},
		{"replay", req{"PUT", assetPath(idA), createBody(shaA), ""}, 200},
		{"break", req{"POST", "/admin/faults", `{"error_rate":1}`, ""}, 200},
		{"faulted", req{"PUT", assetPath(idA), createBody(shaA), ""}, 503},
	})

	ended := spans.Ended()
	last := ended[len(ended)-1]
	if !hasAttr(last.Attributes(), attribute.Bool("fault.injected", true)) {
		t.Errorf("faulted span %q lacks fault.injected=true: %v", last.Name(), last.Attributes())
	}
	if hasAttr(ended[0].Attributes(), attribute.Bool("fault.injected", true)) {
		t.Errorf("healthy span %q has fault.injected", ended[0].Name())
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "keeper.lightroom.requests" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				route, _ := dp.Attributes.Value("route")
				result, _ := dp.Attributes.Value("result")
				got[route.AsString()+"/"+result.AsString()] = dp.Value
			}
		}
	}
	want := map[string]int64{"create_asset/ok": 1, "create_asset/existing": 1, "create_asset/fault": 1}
	if len(got) != len(want) {
		t.Fatalf("metric points = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %d, want %d (all: %v)", k, got[k], v, got)
		}
	}
}

func hasAttr(attrs []attribute.KeyValue, want attribute.KeyValue) bool {
	for _, a := range attrs {
		if a == want {
			return true
		}
	}
	return false
}
