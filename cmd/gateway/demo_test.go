package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeAdmin stands in for lightroom-mock's admin API and records what reached it.
type fakeAdmin struct {
	mu    sync.Mutex
	posts []string // bodies of POST /admin/faults
}

func (f *fakeAdmin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method + " " + r.URL.Path {
	case "GET /admin/faults":
		_, _ = io.WriteString(w, `{"error_rate":0,"status":503,"latency_ms":0}`)
	case "GET /admin/stats":
		_, _ = io.WriteString(w, `{"assets":3,"with_master":3,"create_replays":1,"faults_injected":12}`)
	case "POST /admin/faults":
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.posts = append(f.posts, string(b))
		f.mu.Unlock()
		_, _ = w.Write(b)
	default:
		http.NotFound(w, r)
	}
}

func demoServer(t *testing.T, upstream string) *httptest.Server {
	t.Helper()
	a := &api{bc: NewBroadcaster()}
	if upstream != "" {
		a.demo = newDemoProxy(upstream + "/") // trailing slash is trimmed
	}
	srv := httptest.NewServer(a.routes())
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, method, url, ctype, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestDemoDisabledIs404(t *testing.T) {
	srv := demoServer(t, "")
	for _, m := range []string{http.MethodGet, http.MethodPost} {
		for _, p := range []string{"/api/demo/faults", "/api/demo/stats", "/api/demo/anything"} {
			if code, _ := do(t, m, srv.URL+p, "application/json", "{}"); code != http.StatusNotFound {
				t.Errorf("%s %s = %d, want 404", m, p, code)
			}
		}
	}
}

func TestDemoForwardsReads(t *testing.T) {
	up := &fakeAdmin{}
	upSrv := httptest.NewServer(up)
	defer upSrv.Close()
	srv := demoServer(t, upSrv.URL)

	code, body := do(t, http.MethodGet, srv.URL+"/api/demo/stats", "", "")
	if code != http.StatusOK || !strings.Contains(body, `"assets":3`) {
		t.Fatalf("stats = %d %q", code, body)
	}
	code, body = do(t, http.MethodGet, srv.URL+"/api/demo/faults", "", "")
	if code != http.StatusOK || !strings.Contains(body, `"status":503`) {
		t.Fatalf("faults = %d %q", code, body)
	}
	// Only the two admin paths are reachable; nothing else is proxied.
	if code, _ := do(t, http.MethodGet, srv.URL+"/api/demo/reset", "", ""); code != http.StatusNotFound {
		t.Errorf("GET /api/demo/reset = %d, want 404", code)
	}
	if code, _ := do(t, http.MethodPost, srv.URL+"/api/demo/stats", "application/json", "{}"); code != http.StatusNotFound {
		t.Errorf("POST /api/demo/stats = %d, want 404", code)
	}
}

func TestDemoPostValidates(t *testing.T) {
	up := &fakeAdmin{}
	upSrv := httptest.NewServer(up)
	defer upSrv.Close()
	srv := demoServer(t, upSrv.URL)
	url := srv.URL + "/api/demo/faults"

	tests := []struct {
		name, ctype, body string
		want              int
	}{
		{"break", "application/json", `{"error_rate":1,"status":503,"latency_ms":0}`, 200},
		{"slow, charset param", "application/json; charset=utf-8", `{"error_rate":0,"status":503,"latency_ms":3000}`, 200},
		{"form post", "application/x-www-form-urlencoded", `{"error_rate":1,"status":503,"latency_ms":0}`, 415},
		{"no content type", "", `{"error_rate":1,"status":503,"latency_ms":0}`, 415},
		{"missing field", "application/json", `{"error_rate":1,"status":503}`, 400},
		{"empty object", "application/json", `{}`, 400},
		{"unknown field", "application/json", `{"error_rate":1,"status":503,"latency_ms":0,"drop_db":true}`, 400},
		{"rate too high", "application/json", `{"error_rate":2,"status":503,"latency_ms":0}`, 400},
		{"status not an error", "application/json", `{"error_rate":1,"status":200,"latency_ms":0}`, 400},
		{"latency too long", "application/json", `{"error_rate":0,"status":503,"latency_ms":120001}`, 400},
		{"negative latency", "application/json", `{"error_rate":0,"status":503,"latency_ms":-1}`, 400},
		{"trailing data", "application/json", `{"error_rate":0,"status":503,"latency_ms":0}{}`, 400},
		{"not json", "application/json", `error_rate=1`, 400},
		{"too large", "application/json", `{"error_rate":0,"status":503,"latency_ms":0` + strings.Repeat(" ", demoMaxBody) + `}`, 400},
	}
	for _, tt := range tests {
		if code, body := do(t, http.MethodPost, url, tt.ctype, tt.body); code != tt.want {
			t.Errorf("%s: status %d, want %d (%q)", tt.name, code, tt.want, body)
		}
	}

	// Exactly the two valid configs reached the upstream, re-encoded with
	// only the known fields.
	if len(up.posts) != 2 {
		t.Fatalf("upstream got %d posts, want 2: %q", len(up.posts), up.posts)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(up.posts[1]), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got["latency_ms"] != 3000.0 || got["error_rate"] != 0.0 || got["status"] != 503.0 {
		t.Errorf("forwarded body = %q", up.posts[1])
	}
}

func TestDemoUpstreamFailures(t *testing.T) {
	// Upstream down: connection refused becomes 502.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	srv := demoServer(t, deadURL)
	if code, _ := do(t, http.MethodGet, srv.URL+"/api/demo/stats", "", ""); code != http.StatusBadGateway {
		t.Errorf("dead upstream = %d, want 502", code)
	}

	// Upstream answers, but not with JSON: 502, never relayed.
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "<html>proxy error</html>")
	}))
	defer html.Close()
	srv = demoServer(t, html.URL)
	code, body := do(t, http.MethodGet, srv.URL+"/api/demo/faults", "", "")
	if code != http.StatusBadGateway || strings.Contains(body, "<html>") {
		t.Errorf("non-JSON upstream = %d %q, want 502 without the body", code, body)
	}

	// Upstream rejects with a JSON error: status and body are relayed.
	reject := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"code":400,"description":"nope"}`)
	}))
	defer reject.Close()
	srv = demoServer(t, reject.URL)
	code, body = do(t, http.MethodPost, srv.URL+"/api/demo/faults", "application/json", `{"error_rate":0,"status":503,"latency_ms":0}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "nope") {
		t.Errorf("upstream 400 = %d %q", code, body)
	}
}

func TestDemoAdminURL(t *testing.T) {
	t.Setenv("DEMO_CONTROLS", "")
	t.Setenv("LIGHTROOM_ADMIN_URL", "")
	if got := demoAdminURL(); got != "" {
		t.Errorf("off: %q", got)
	}
	t.Setenv("DEMO_CONTROLS", "true") // only "1" enables it
	if got := demoAdminURL(); got != "" {
		t.Errorf("true: %q", got)
	}
	t.Setenv("DEMO_CONTROLS", "1")
	if got := demoAdminURL(); got != "http://lightroom-mock:8080" {
		t.Errorf("default: %q", got)
	}
	t.Setenv("LIGHTROOM_ADMIN_URL", "http://127.0.0.1:8091")
	if got := demoAdminURL(); got != "http://127.0.0.1:8091" {
		t.Errorf("override: %q", got)
	}
}
