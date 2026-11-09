package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"go.opentelemetry.io/otel/trace/noop"
)

// writeFiles creates empty files (and their directories) under root.
func writeFiles(t *testing.T, root string, names ...string) {
	t.Helper()
	for _, n := range names {
		p := filepath.Join(root, n)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func testSets(t *testing.T) string {
	root := t.TempDir()
	writeFiles(t, root,
		"zebra/b.jpg", "zebra/a.JPG",
		"alpha/frame-10.jpg", "alpha/frame-02.jpeg", "alpha/frame-01.jpg",
		"alpha/notes.txt", "alpha/.hidden.jpg", "alpha/sub/nested.jpg",
		"empty/README.md",
		".git/x.jpg",
	)
	writeFiles(t, root, "README.md")
	return root
}

func TestListSets(t *testing.T) {
	got, err := listSets(testSets(t))
	if err != nil {
		t.Fatal(err)
	}
	want := []setInfo{{"alpha", 3}, {"zebra", 2}}
	if !slices.Equal(got, want) {
		t.Fatalf("listSets = %v, want %v", got, want)
	}
}

func TestSetFramesOrder(t *testing.T) {
	root := testSets(t)
	frames, err := setFrames(root, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range frames {
		names = append(names, filepath.Base(f))
	}
	want := []string{"frame-01.jpg", "frame-02.jpeg", "frame-10.jpg"}
	if !slices.Equal(names, want) {
		t.Fatalf("frames = %v, want %v", names, want)
	}
}

func TestSetFramesRejects(t *testing.T) {
	root := testSets(t)
	for _, name := range []string{"", "..", "../etc", "alpha/sub", ".git", "empty", "missing"} {
		if _, err := setFrames(root, name); !errors.Is(err, errNoSet) {
			t.Errorf("setFrames(%q) err = %v, want errNoSet", name, err)
		}
	}
}

func TestPlanUploads(t *testing.T) {
	frames := []string{"/s/b/KS_0001.jpg", "/s/b/KS_0002.jpg"}
	if got := planUploads(frames, false); len(got) != 2 || got[1].Remote != "KS_0002.jpg" {
		t.Fatalf("plan without dup = %v", got)
	}
	got := planUploads(frames, true)
	last := got[len(got)-1]
	if len(got) != 3 || last.Local != frames[0] || last.Remote != "KS_0001-dup.jpg" {
		t.Fatalf("plan with dup = %v", got)
	}
}

func TestHTTP(t *testing.T) {
	s := &shooter{setsDir: testSets(t), tracer: noop.NewTracerProvider().Tracer("test")}
	h := newMux(s)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/sets", nil))
	var body struct{ Sets []setInfo }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); rec.Code != 200 || err != nil || len(body.Sets) != 2 {
		t.Fatalf("GET /sets = %d %s", rec.Code, rec.Body)
	}

	for target, want := range map[string]int{
		"/shoot":                 http.StatusBadRequest,
		"/shoot?set=missing":     http.StatusNotFound,
		"/shoot?set=..%2Fetc":    http.StatusNotFound,
		"/shoot?set=empty&dup=1": http.StatusNotFound,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", target, nil))
		if rec.Code != want {
			t.Errorf("POST %s = %d, want %d (%s)", target, rec.Code, want, rec.Body)
		}
	}
}
