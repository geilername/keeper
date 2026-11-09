package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/geilername/keeper/internal/lightroom"
	"github.com/geilername/keeper/internal/lightroom/lightroomtest"
	"github.com/geilername/keeper/internal/lrlink"
)

type memAuth struct {
	mu  sync.Mutex
	row *authRow
}

func (m *memAuth) LoadLightroomAuth(context.Context) (authRow, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.row == nil {
		return authRow{}, false, nil
	}
	return *m.row, true, nil
}

func (m *memAuth) SaveLightroomAuth(_ context.Context, r authRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.row = &r
	return nil
}

type memLink struct {
	mu     sync.Mutex
	tokens []lrlink.Token
	target *lrlink.Target
}

func (m *memLink) PutToken(_ context.Context, t lrlink.Token) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens = append(m.tokens, t)
	return nil
}

func (m *memLink) PutTarget(_ context.Context, t lrlink.Target) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.target = &t
	return nil
}

func (m *memLink) Target(context.Context) (lrlink.Target, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.target == nil {
		return lrlink.Target{}, false, nil
	}
	return *m.target, true, nil
}

func (m *memLink) last() lrlink.Token {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.tokens) == 0 {
		return lrlink.Token{}
	}
	return m.tokens[len(m.tokens)-1]
}

type lrEnv struct {
	srv   *httptest.Server
	ims   *lightroomtest.IMS
	adobe *lightroomtest.Adobe
	auth  *memAuth
	link  *memLink
	lr    *lightroomAuth
	now   time.Time
	mu    sync.Mutex
}

func (e *lrEnv) clock() time.Time        { e.mu.Lock(); defer e.mu.Unlock(); return e.now }
func (e *lrEnv) advance(d time.Duration) { e.mu.Lock(); e.now = e.now.Add(d); e.mu.Unlock() }

func newLREnv(t *testing.T) *lrEnv {
	t.Helper()
	e := &lrEnv{
		ims:   lightroomtest.NewIMS("client-id", "client-secret", "good-code"),
		adobe: lightroomtest.NewAdobe("client-id", "access-1"),
		auth:  &memAuth{}, link: &memLink{},
		now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC),
	}
	t.Cleanup(e.ims.Close)
	t.Cleanup(e.adobe.Close)
	cfg := lrConfig{
		mode: "adobe", clientID: "client-id", clientSecret: "client-secret",
		redirectURI: defaultRedirectURI, imsURL: e.ims.URL, apiURL: e.adobe.URL,
		tokenKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)),
	}
	lr, err := newLightroomAuth(cfg, e.auth, e.link)
	if err != nil {
		t.Fatal(err)
	}
	lr.now = e.clock
	lr.ims.Now = e.clock
	e.lr = lr
	e.srv = httptest.NewServer((&api{bc: NewBroadcaster(), lr: lr}).routes())
	t.Cleanup(e.srv.Close)
	return e
}

func noRedirect(jar http.CookieJar) *http.Client {
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// connect runs login → (Adobe) → callback with the browser's cookie jar.
func (e *lrEnv) connect(t *testing.T) {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := noRedirect(jar)
	res, err := c.Get(e.srv.URL + "/auth/lightroom/login")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound {
		t.Fatalf("login: %d", res.StatusCode)
	}
	loc, _ := url.Parse(res.Header.Get("Location"))
	state := loc.Query().Get("state")
	if !strings.HasPrefix(loc.String(), e.ims.URL+"/ims/authorize/v2?") || len(state) < 40 {
		t.Fatalf("login redirect %s", loc)
	}
	res, err = c.Get(e.srv.URL + "/auth/lightroom/callback?code=good-code&state=" + url.QueryEscape(state))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/?lightroom=connected" {
		t.Fatalf("callback: %d → %s", res.StatusCode, res.Header.Get("Location"))
	}
}

func (e *lrEnv) status(t *testing.T) lrStatus {
	t.Helper()
	code, body := do(t, http.MethodGet, e.srv.URL+"/api/lightroom/status", "", "")
	if code != 200 {
		t.Fatalf("status: %d %s", code, body)
	}
	var s lrStatus
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLightroomNotConfigured(t *testing.T) {
	srv := httptest.NewServer((&api{bc: NewBroadcaster(), lrMode: "mock"}).routes())
	defer srv.Close()
	code, body := do(t, http.MethodGet, srv.URL+"/api/lightroom/status", "", "")
	if code != 200 || !strings.Contains(body, `"state":"not_configured"`) || !strings.Contains(body, `"mode":"mock"`) {
		t.Fatalf("status %d %s", code, body)
	}
	for _, p := range []string{"/auth/lightroom/login", "/api/lightroom/collections"} {
		if code, _ := do(t, http.MethodGet, srv.URL+p, "", ""); code != http.StatusServiceUnavailable {
			t.Errorf("%s: %d", p, code)
		}
	}
}

func TestLightroomConnectStoresSealedTokens(t *testing.T) {
	e := newLREnv(t)
	if s := e.status(t); s.State != lrNotConnected || s.LoginURL != "/auth/lightroom/login" {
		t.Fatalf("before: %+v", s)
	}
	e.connect(t)

	s := e.status(t)
	if s.State != lrConnected || s.Account != "Ada Lovelace" || s.ExpiresAt == nil || !s.ExpiresAt.Equal(e.clock().Add(24*time.Hour)) {
		t.Fatalf("after: %+v", s)
	}
	row, _, _ := e.auth.LoadLightroomAuth(context.Background())
	if bytes.Contains(row.Sealed, []byte("access-1")) || bytes.Contains(row.Sealed, []byte("refresh-1")) {
		t.Fatal("tokens stored in the clear")
	}
	if tok := e.link.last(); tok.AccessToken != "access-1" || tok.AccountID != e.adobe.AccountID {
		t.Fatalf("token for the publisher: %+v", tok)
	}
}

func TestLightroomCallbackChecksState(t *testing.T) {
	e := newLREnv(t)
	jar, _ := cookiejar.New(nil)
	c := noRedirect(jar)
	res, _ := c.Get(e.srv.URL + "/auth/lightroom/login")
	res.Body.Close()

	for name, q := range map[string]string{
		"forged state": "code=good-code&state=forged",
		"no state":     "code=good-code",
	} {
		res, err := c.Get(e.srv.URL + "/auth/lightroom/callback?" + q)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d", name, res.StatusCode)
		}
	}
	// Without the cookie (another browser), even the right-looking state fails.
	res, _ = http.Get(e.srv.URL + "/auth/lightroom/callback?code=good-code&state=x")
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("cookieless callback: %d", res.StatusCode)
	}
	if e.status(t).State != lrNotConnected {
		t.Fatal("connected through a forged callback")
	}
}

func TestLightroomRefreshBeforeExpiry(t *testing.T) {
	e := newLREnv(t)
	e.connect(t)
	ctx := context.Background()

	// Well before expiry: no refresh.
	e.lr.tick(ctx, false)
	if e.ims.Refreshes() != 0 {
		t.Fatal("refreshed too early")
	}
	// Inside the margin: refreshed, re-sealed, handed to the publisher.
	e.advance(24*time.Hour - 5*time.Minute)
	e.lr.tick(ctx, false)
	if e.ims.Refreshes() != 1 || e.link.last().AccessToken != "access-2" {
		t.Fatalf("refreshes %d, publisher token %+v", e.ims.Refreshes(), e.link.last())
	}
	if s := e.status(t); s.State != lrConnected || !s.ExpiresAt.Equal(e.clock().Add(24*time.Hour)) {
		t.Fatalf("after refresh: %+v", s)
	}
	// A fresh gateway (cache empty) reads the sealed row back.
	e.lr.cur = nil
	if tok, err := e.lr.AccessToken(ctx); err != nil || tok != "access-2" {
		t.Fatalf("reload: %q %v", tok, err)
	}

	// The grant is revoked and the token runs out: expired, and no token.
	e.ims.Revoke()
	e.advance(24 * time.Hour)
	if _, err := e.lr.AccessToken(ctx); err != lightroom.ErrNotConnected {
		t.Fatalf("revoked: %v", err)
	}
	if s := e.status(t); s.State != lrExpired {
		t.Fatalf("state %q, want expired", s.State)
	}
}

func TestLightroomMaintainRepublishesAtStart(t *testing.T) {
	e := newLREnv(t)
	e.connect(t)
	n := len(e.link.tokens)
	e.lr.cur = nil // restart
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.lr.maintain(ctx, time.Hour) // runs the start tick, then sees ctx done
	if len(e.link.tokens) != n+1 {
		t.Fatalf("tokens published %d, want %d", len(e.link.tokens), n+1)
	}
}

func TestLightroomCollectionsAndTarget(t *testing.T) {
	e := newLREnv(t)
	e.adobe.AddAlbum(lightroomtest.Album{ID: "set1", Name: "2026", Subtype: "collection_set"})
	e.adobe.AddAlbum(lightroomtest.Album{ID: "set2", Name: "Conferences", Subtype: "collection_set", ParentID: "set1"})
	e.adobe.AddAlbum(lightroomtest.Album{ID: "col1", Name: "KubeCon", Subtype: "collection", ParentID: "set2"})
	e.adobe.AddAlbum(lightroomtest.Album{ID: "col2", Name: "Talks", Subtype: "collection"})
	e.adobe.AddAlbum(lightroomtest.Album{ID: "col3", Name: "Zoo", Subtype: "collection"})

	// Not connected yet: 409, so the console can say "connect first".
	if code, _ := do(t, http.MethodGet, e.srv.URL+"/api/lightroom/collections", "", ""); code != http.StatusConflict {
		t.Fatalf("collections before connect: %d", code)
	}
	e.connect(t)

	code, body := do(t, http.MethodGet, e.srv.URL+"/api/lightroom/collections", "", "")
	var cols []lightroom.Collection
	if code != 200 || json.Unmarshal([]byte(body), &cols) != nil || len(cols) != 3 || cols[0].Path != "2026 / Conferences / KubeCon" {
		t.Fatalf("collections %d %s", code, body)
	}

	put := func(ctype, body string) (int, string) {
		return do(t, http.MethodPut, e.srv.URL+"/api/lightroom/target", ctype, body)
	}
	if code, _ := put("text/plain", `{"album_id":"col1"}`); code != http.StatusUnsupportedMediaType {
		t.Fatalf("form-like put: %d", code)
	}
	if code, _ := put("application/json", `{"album_id":"nope"}`); code != http.StatusNotFound {
		t.Fatalf("unknown album: %d", code)
	}
	if code, _ := put("application/json", `{"album":"col1"}`); code != http.StatusBadRequest {
		t.Fatalf("bad body: %d", code)
	}
	if code, body := put("application/json; charset=utf-8", `{"album_id":"col1"}`); code != 200 {
		t.Fatalf("put: %d %s", code, body)
	}
	s := e.status(t)
	if s.Target == nil || s.Target.AlbumID != "col1" || s.Target.Name != "KubeCon" || s.Target.Path != "2026 / Conferences / KubeCon" {
		t.Fatalf("status target %+v", s.Target)
	}
}

func TestOAuthRoutesAreNotTraced(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/auth/lightroom/callback?code=secret", nil)
	if traced(r) {
		t.Fatal("the callback (with the code in its query) would be traced")
	}
	if !traced(httptest.NewRequest(http.MethodGet, "/api/lightroom/status", nil)) {
		t.Fatal("status should be traced")
	}
}
