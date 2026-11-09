package main

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestThumbSHA(t *testing.T) {
	tests := []struct {
		name string
		ok   bool
	}{
		{testSHA + ".jpg", true},
		{testSHA, false},
		{testSHA + ".png", false},
		{testSHA + ".jpg.jpg", false},
		{strings.ToUpper(testSHA) + ".jpg", false},
		{"../" + testSHA[3:] + ".jpg", false},
		{"..%2f..%2fetc%2fpasswd.jpg", false},
		{"/etc/passwd.jpg", false},
		{testSHA[:32] + "/" + testSHA[33:] + ".jpg", false},
		{".jpg", false},
	}
	for _, tt := range tests {
		sha, ok := thumbSHA(tt.name)
		if ok != tt.ok {
			t.Errorf("thumbSHA(%q) ok = %v, want %v", tt.name, ok, tt.ok)
		}
		if ok && sha != testSHA {
			t.Errorf("thumbSHA(%q) = %q", tt.name, sha)
		}
	}
}

func TestThumbHandler(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SPOOL_DIR", dir)
	if err := os.MkdirAll(filepath.Join(dir, "thumbs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "thumbs", testSHA+".jpg"), []byte("jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A file next to the spool that path tricks would try to reach.
	if err := os.WriteFile(filepath.Join(dir, "secret.jpg"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&api{bc: NewBroadcaster()}).routes())
	defer srv.Close()

	tests := []struct {
		path   string
		status int
	}{
		{"/thumbs/" + testSHA + ".jpg", http.StatusOK},
		{"/thumbs/" + strings.Repeat("a", 64) + ".jpg", http.StatusNotFound}, // valid, missing
		{"/thumbs/secret.jpg", http.StatusNotFound},
		{"/thumbs/..%2Fsecret.jpg", http.StatusNotFound},
		{"/thumbs/" + strings.ToUpper(testSHA) + ".jpg", http.StatusNotFound},
	}
	for _, tt := range tests {
		res, err := http.Get(srv.URL + tt.path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != tt.status {
			t.Errorf("GET %s = %d, want %d", tt.path, res.StatusCode, tt.status)
		}
		if res.StatusCode == http.StatusOK {
			if cc := res.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
				t.Errorf("Cache-Control = %q", cc)
			}
			if ct := res.Header.Get("Content-Type"); ct != "image/jpeg" {
				t.Errorf("Content-Type = %q", ct)
			}
		}
	}
}

func TestTraceLink(t *testing.T) {
	id, url := traceLinker{}.link(testSHA)
	if id != testSHA[:32] || url != "" {
		t.Errorf("no template: id=%q url=%q", id, url)
	}
	_, url = traceLinker{tmpl: "https://app.dash0.com/traces/{trace_id}?ds=x"}.link(testSHA)
	if url != "https://app.dash0.com/traces/"+testSHA[:32]+"?ds=x" {
		t.Errorf("url = %q", url)
	}
}

func TestTraced(t *testing.T) {
	for path, want := range map[string]bool{
		"/api/photos":                 true,
		"/api/photos/" + testSHA:      true,
		"/api/events":                 false,
		"/healthz":                    false,
		"/thumbs/" + testSHA + ".jpg": false,
	} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if got := traced(r); got != want {
			t.Errorf("traced(%s) = %v, want %v", path, got, want)
		}
	}
}

func TestBroadcasterDropsSlowClient(t *testing.T) {
	b := NewBroadcaster()
	slow, _ := b.Subscribe()
	for i := 0; i <= clientBuffer; i++ {
		b.Publish(i)
	}
	if b.Clients() != 0 {
		t.Fatalf("slow client still subscribed")
	}
	n := 0
	for range slow { // closed after the buffered messages
		n++
	}
	if n != clientBuffer {
		t.Errorf("got %d buffered messages, want %d", n, clientBuffer)
	}
	b.Unsubscribe(slow) // must not panic after a drop
}

func TestSSEDeliversAndCleansUp(t *testing.T) {
	b := NewBroadcaster()
	srv := httptest.NewServer(http.HandlerFunc(b.ServeSSE))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	waitFor(t, func() bool { return b.Clients() == 1 })

	b.Publish(map[string]string{"sha256": "x"})
	sc := bufio.NewScanner(res.Body)
	var got []string
	for sc.Scan() {
		got = append(got, sc.Text())
		if strings.HasPrefix(sc.Text(), "data: ") {
			break
		}
	}
	if want := `data: {"sha256":"x"}`; got[len(got)-1] != want {
		t.Errorf("last line %q, want %q (all: %q)", got[len(got)-1], want, got)
	}

	cancel() // client goes away: the handler must unsubscribe
	waitFor(t, func() bool { return b.Clients() == 0 })
}

func TestSSEClosedOnShutdown(t *testing.T) {
	b := NewBroadcaster()
	srv := httptest.NewServer(http.HandlerFunc(b.ServeSSE))
	defer srv.Close()
	res, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	waitFor(t, func() bool { return b.Clients() == 1 })

	b.Close()
	done := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
		}
		close(done)
	}()
	select {
	case <-done: // server ended the stream
	case <-time.After(2 * time.Second):
		t.Fatal("stream still open after Close")
	}
	if _, ok := b.Subscribe(); ok {
		t.Error("subscribe after Close succeeded")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// WebP thumbnails are served as WebP; photos from before WebP keep their JPEG,
// also when the console asks for <sha>.webp.
func TestThumbWebPAndLegacyJPEG(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SPOOL_DIR", dir)
	webpSHA := strings.Repeat("b", 64)
	if err := os.MkdirAll(filepath.Join(dir, "thumbs"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{webpSHA + ".webp": "RIFFwebp", testSHA + ".jpg": "jpeg"} {
		if err := os.WriteFile(filepath.Join(dir, "thumbs", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer((&api{bc: NewBroadcaster()}).routes())
	defer srv.Close()

	for path, want := range map[string]string{
		"/thumbs/" + webpSHA + ".webp": "image/webp",
		"/thumbs/" + testSHA + ".webp": "image/jpeg", // legacy fallback
		"/thumbs/" + testSHA + ".jpg":  "image/jpeg",
	} {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != want {
			t.Errorf("GET %s = %d %q, want 200 %q", path, res.StatusCode, res.Header.Get("Content-Type"), want)
		}
	}
	if got := thumbURL(webpSHA); got != "/thumbs/"+webpSHA+".webp" {
		t.Errorf("thumbURL = %s", got)
	}
}
