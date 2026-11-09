package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/geilername/keeper/internal/lightroom"
	"github.com/geilername/keeper/internal/lrlink"
)

// Adobe Lightroom connection. The gateway owns the OAuth grant:
//
//	GET  /auth/lightroom/login     → 302 to Adobe IMS (state in a cookie)
//	GET  /auth/lightroom/callback  ← IMS; exchanges the code, stores tokens
//	GET  /api/lightroom/status     connection state, account, expiry, target
//	GET  /api/lightroom/collections the user's collections, with set paths
//	PUT  /api/lightroom/target     {"album_id": …} picks the collection
//
// Access and refresh tokens are sealed with AES-GCM (KEEPER_TOKEN_KEY) in
// gateway.lightroom_auth. The access token is refreshed before it expires,
// and each one is handed to the publisher, sealed, through the
// keeper-lightroom bucket together with the target (internal/lrlink).

// lrConfig comes from ADOBE_CLIENT_ID, ADOBE_CLIENT_SECRET,
// ADOBE_REDIRECT_URI, KEEPER_TOKEN_KEY (and, for tests or a proxy,
// ADOBE_IMS_URL and LIGHTROOM_URL).
type lrConfig struct {
	mode         string // LIGHTROOM_MODE of the stack: mock | adobe (shown in the console)
	clientID     string
	clientSecret string
	redirectURI  string
	imsURL       string
	apiURL       string
	tokenKey     string
}

const defaultRedirectURI = "https://localhost:8443/auth/lightroom/callback"

func loadLRConfig() lrConfig {
	c := lrConfig{
		mode:         env("LIGHTROOM_MODE", "mock"),
		clientID:     os.Getenv("ADOBE_CLIENT_ID"),
		clientSecret: os.Getenv("ADOBE_CLIENT_SECRET"),
		redirectURI:  env("ADOBE_REDIRECT_URI", defaultRedirectURI),
		imsURL:       env("ADOBE_IMS_URL", lightroom.DefaultIMSURL),
		apiURL:       lightroom.DefaultAdobeURL,
		tokenKey:     os.Getenv("KEEPER_TOKEN_KEY"),
	}
	if c.mode == "adobe" {
		c.apiURL = env("LIGHTROOM_URL", lightroom.DefaultAdobeURL)
	}
	return c
}

// configured: client ID and secret are set. KEEPER_TOKEN_KEY is checked
// when the sealer is built.
func (c lrConfig) configured() bool { return c.clientID != "" && c.clientSecret != "" }

// authRow is gateway.lightroom_auth.
type authRow struct {
	Sealed      []byte // AES-GCM(JSON sealedGrant)
	ExpiresAt   time.Time
	AccountID   string
	AccountName string
	UpdatedAt   time.Time
}

type sealedGrant struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

// grantAAD binds a sealed row to its table.
var grantAAD = []byte("keeper/gateway.lightroom_auth/v1")

type authStore interface {
	LoadLightroomAuth(ctx context.Context) (authRow, bool, error)
	SaveLightroomAuth(ctx context.Context, row authRow) error
}

type linkStore interface {
	PutToken(ctx context.Context, t lrlink.Token) error
	PutTarget(ctx context.Context, t lrlink.Target) error
	Target(ctx context.Context) (lrlink.Target, bool, error)
}

// adobeAPI is the part of lightroom.Adobe the gateway uses.
type adobeAPI interface {
	Account(ctx context.Context) (lightroom.Account, error)
	ListCollections(ctx context.Context) ([]lightroom.Collection, error)
	Reset()
}

type grant struct {
	sealedGrant
	expiresAt   time.Time
	accountID   string
	accountName string
}

// lightroomAuth holds the connection. Its zero value is not usable; a nil
// *lightroomAuth means "not configured" and every handler says so.
type lightroomAuth struct {
	cfg    lrConfig
	ims    *lightroom.IMS
	sealer *lrlink.Sealer
	store  authStore
	link   linkStore
	api    adobeAPI // uses this as its TokenSource
	// newAPI builds a client for one token (the account lookup right
	// after the code exchange, before anything is stored).
	newAPI func(lightroom.TokenSource) adobeAPI
	now    func() time.Time

	mu  sync.Mutex // serialises loads and refreshes
	cur *grant     // nil: not loaded or not connected
}

// Refresh this long before the access token expires (IMS hands out 24 h
// tokens); the publisher treats a token as dead 30 s before expiry.
const refreshMargin = 10 * time.Minute

func newLightroomAuth(cfg lrConfig, store authStore, link linkStore) (*lightroomAuth, error) {
	sealer, err := lrlink.NewSealer(cfg.tokenKey)
	if err != nil {
		return nil, err
	}
	a := &lightroomAuth{
		cfg: cfg, sealer: sealer, store: store, link: link, now: time.Now,
		ims: &lightroom.IMS{
			BaseURL: cfg.imsURL, ClientID: cfg.clientID, ClientSecret: cfg.clientSecret,
			RedirectURI: cfg.redirectURI,
		},
	}
	a.newAPI = func(ts lightroom.TokenSource) adobeAPI {
		return lightroom.NewAdobe(cfg.apiURL, cfg.clientID, ts, 15*time.Second)
	}
	a.api = a.newAPI(a)
	return a, nil
}

// load returns the stored grant (cached), or nil when nobody connected.
// Caller holds a.mu.
func (a *lightroomAuth) load(ctx context.Context) (*grant, error) {
	if a.cur != nil {
		return a.cur, nil
	}
	row, ok, err := a.store.LoadLightroomAuth(ctx)
	if err != nil || !ok {
		return nil, err
	}
	plain, err := a.sealer.Open(row.Sealed, grantAAD)
	if err != nil {
		return nil, err
	}
	g := &grant{expiresAt: row.ExpiresAt, accountID: row.AccountID, accountName: row.AccountName}
	if err := json.Unmarshal(plain, &g.sealedGrant); err != nil {
		return nil, err
	}
	a.cur = g
	return g, nil
}

// save seals g into Postgres, then hands the access token to the publisher.
// Caller holds a.mu.
func (a *lightroomAuth) save(ctx context.Context, g *grant) error {
	plain, err := json.Marshal(g.sealedGrant)
	if err != nil {
		return err
	}
	if err := a.store.SaveLightroomAuth(ctx, authRow{
		Sealed: a.sealer.Seal(plain, grantAAD), ExpiresAt: g.expiresAt,
		AccountID: g.accountID, AccountName: g.accountName,
	}); err != nil {
		return err
	}
	a.cur = g
	return a.publish(ctx, g)
}

func (a *lightroomAuth) publish(ctx context.Context, g *grant) error {
	return a.link.PutToken(ctx, lrlink.Token{AccessToken: g.AccessToken, ExpiresAt: g.expiresAt, AccountID: g.accountID})
}

// AccessToken makes lightroomAuth the gateway's TokenSource: the stored
// token, refreshed first if it is about to expire.
func (a *lightroomAuth) AccessToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	g, err := a.load(ctx)
	if err != nil {
		return "", err
	}
	if g == nil {
		return "", lightroom.ErrNotConnected
	}
	if a.now().Add(refreshMargin).After(g.expiresAt) {
		if err := a.refresh(ctx, g); err != nil {
			slog.WarnContext(ctx, "lightroom token refresh failed", "err", err)
			if !a.now().Before(g.expiresAt) {
				return "", lightroom.ErrNotConnected
			}
		}
		g = a.cur
	}
	return g.AccessToken, nil
}

// refresh trades the refresh token for a new access token. Caller holds a.mu.
func (a *lightroomAuth) refresh(ctx context.Context, g *grant) error {
	if g.RefreshToken == "" {
		return errors.New("no refresh token (offline_access not granted)")
	}
	ts, err := a.ims.Refresh(ctx, g.RefreshToken)
	if err != nil {
		return err
	}
	next := *g
	next.AccessToken, next.RefreshToken, next.expiresAt = ts.AccessToken, ts.RefreshToken, ts.ExpiresAt
	if err := a.save(ctx, &next); err != nil {
		return err
	}
	slog.InfoContext(ctx, "lightroom token refreshed", "expires_at", ts.ExpiresAt)
	return nil
}

// maintain keeps the token fresh until ctx ends: once at start (which also
// re-publishes a valid token to the bucket, e.g. after NATS lost it) and
// then every interval.
func (a *lightroomAuth) maintain(ctx context.Context, every time.Duration) {
	a.tick(ctx, true)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.tick(ctx, false)
		}
	}
}

func (a *lightroomAuth) tick(ctx context.Context, republish bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	g, err := a.load(ctx)
	if err != nil || g == nil {
		if err != nil {
			slog.WarnContext(ctx, "lightroom grant unreadable", "err", err)
		}
		return
	}
	if a.now().Add(refreshMargin).After(g.expiresAt) {
		if err := a.refresh(ctx, g); err != nil {
			slog.WarnContext(ctx, "lightroom token refresh failed", "err", err, "expires_at", g.expiresAt)
		}
		return
	}
	if republish {
		if err := a.publish(ctx, g); err != nil {
			slog.WarnContext(ctx, "could not hand the lightroom token to the publisher", "err", err)
		}
	}
}

// --- HTTP ---

const stateCookie = "keeper_lr_state"

func (a *api) registerLightroom(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/lightroom/login", a.lrLogin)
	mux.HandleFunc("GET /auth/lightroom/callback", a.lrCallback)
	mux.HandleFunc("GET /api/lightroom/status", a.lrStatus)
	mux.HandleFunc("GET /api/lightroom/collections", a.lrCollections)
	mux.HandleFunc("PUT /api/lightroom/target", a.lrSetTarget)
}

func (a *api) lrLogin(w http.ResponseWriter, r *http.Request) {
	if a.lr == nil {
		http.Error(w, "Adobe Lightroom is not configured (ADOBE_CLIENT_ID, ADOBE_CLIENT_SECRET, KEEPER_TOKEN_KEY)", http.StatusServiceUnavailable)
		return
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		a.fail(w, r, err)
		return
	}
	state := base64.RawURLEncoding.EncodeToString(b)
	// The login may be on the HTTP port and the callback on the HTTPS one:
	// same host, and cookies ignore the port. Lax: it must survive the
	// top-level redirect back from Adobe. Secure only when set over TLS,
	// or a browser on the HTTP port would drop it.
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: state, Path: "/auth/lightroom/", MaxAge: 600,
		HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode,
	})
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, a.lr.ims.AuthURL(state), http.StatusFound)
}

func (a *api) lrCallback(w http.ResponseWriter, r *http.Request) {
	if a.lr == nil {
		http.Error(w, "Adobe Lightroom is not configured", http.StatusServiceUnavailable)
		return
	}
	c, err := r.Cookie(stateCookie)
	q := r.URL.Query()
	state := q.Get("state")
	// Clear it either way: a state is good for one try.
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Path: "/auth/lightroom/", MaxAge: -1,
		HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode})
	if err != nil || state == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(state)) != 1 {
		http.Error(w, "Lightroom sign-in failed: the state does not match this browser. Start again from the console.", http.StatusBadRequest)
		return
	}
	if e := q.Get("error"); e != "" {
		slog.WarnContext(r.Context(), "lightroom sign-in declined", "error", e, "description", q.Get("error_description"))
		http.Redirect(w, r, "/?lightroom=declined", http.StatusSeeOther)
		return
	}
	code := q.Get("code")
	if code == "" {
		http.Error(w, "Lightroom sign-in failed: no code", http.StatusBadRequest)
		return
	}
	if err := a.lr.connect(r.Context(), code); err != nil {
		slog.ErrorContext(r.Context(), "lightroom connect failed", "err", err)
		http.Redirect(w, r, "/?lightroom=failed", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/?lightroom=connected", http.StatusSeeOther)
}

// connect exchanges the code, names the account and stores the grant.
func (a *lightroomAuth) connect(ctx context.Context, code string) error {
	ts, err := a.ims.Exchange(ctx, code)
	if err != nil {
		return err
	}
	acc, err := a.newAPI(lightroom.StaticToken(ts.AccessToken)).Account(ctx)
	if err != nil {
		return fmt.Errorf("read account: %w", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	g := &grant{
		sealedGrant: sealedGrant{AccessToken: ts.AccessToken, RefreshToken: ts.RefreshToken},
		expiresAt:   ts.ExpiresAt, accountID: acc.ID, accountName: acc.DisplayName(),
	}
	if prev, _ := a.load(ctx); prev == nil || prev.accountID != g.accountID {
		a.api.Reset()
	}
	if err := a.save(ctx, g); err != nil {
		return err
	}
	slog.InfoContext(ctx, "lightroom connected", "account", g.accountName, "expires_at", g.expiresAt,
		"refresh_token", g.RefreshToken != "", "entitlement", acc.Entitlement.Status)
	return nil
}

// Connection states for /api/lightroom/status.
const (
	lrNotConfigured = "not_configured"
	lrNotConnected  = "not_connected"
	lrConnected     = "connected"
	lrExpired       = "expired"
)

type lrStatus struct {
	State     string         `json:"state"`
	Mode      string         `json:"mode"` // mock | adobe: where the publisher sends photos
	Account   string         `json:"account,omitempty"`
	ExpiresAt *time.Time     `json:"expires_at,omitempty"`
	Target    *lrlink.Target `json:"target,omitempty"`
	LoginURL  string         `json:"login_url,omitempty"`
}

func (a *api) lrStatus(w http.ResponseWriter, r *http.Request) {
	if a.lr == nil {
		writeJSON(w, lrStatus{State: lrNotConfigured, Mode: a.lrMode})
		return
	}
	writeJSON(w, a.lr.status(r.Context()))
}

func (a *lightroomAuth) status(ctx context.Context) lrStatus {
	s := lrStatus{State: lrNotConnected, Mode: a.cfg.mode, LoginURL: "/auth/lightroom/login"}
	a.mu.Lock()
	g, err := a.load(ctx)
	a.mu.Unlock()
	if err != nil {
		slog.WarnContext(ctx, "lightroom grant unreadable", "err", err)
	}
	if g != nil {
		s.Account = g.accountName
		exp := g.expiresAt.UTC()
		s.ExpiresAt = &exp
		s.State = lrConnected
		if !a.now().Before(g.expiresAt) {
			s.State = lrExpired
		}
	}
	if t, ok, err := a.link.Target(ctx); err != nil {
		slog.WarnContext(ctx, "reading lightroom target", "err", err)
	} else if ok {
		s.Target = &t
	}
	return s
}

func (a *api) lrCollections(w http.ResponseWriter, r *http.Request) {
	if a.lr == nil {
		http.Error(w, "Adobe Lightroom is not configured", http.StatusServiceUnavailable)
		return
	}
	cols, err := a.lr.api.ListCollections(r.Context())
	if err != nil {
		a.lrFail(w, r, err)
		return
	}
	writeJSON(w, cols)
}

// lrFail answers 409 when the user must connect first, 502 otherwise.
func (a *api) lrFail(w http.ResponseWriter, r *http.Request, err error) {
	if class, _ := lightroom.Classify(err); class == lightroom.ClassAuth {
		http.Error(w, "Lightroom is not connected", http.StatusConflict)
		return
	}
	slog.ErrorContext(r.Context(), "lightroom request failed", "path", r.URL.Path, "err", err)
	http.Error(w, "Lightroom: "+err.Error(), http.StatusBadGateway)
}

func (a *api) lrSetTarget(w http.ResponseWriter, r *http.Request) {
	if a.lr == nil {
		http.Error(w, "Adobe Lightroom is not configured", http.StatusServiceUnavailable)
		return
	}
	// JSON only: a cross-site form cannot send it without a preflight.
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	var req struct {
		AlbumID string `json:"album_id"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.AlbumID == "" {
		http.Error(w, `body must be {"album_id": "<collection id>"}`, http.StatusBadRequest)
		return
	}
	cols, err := a.lr.api.ListCollections(r.Context())
	if err != nil {
		a.lrFail(w, r, err)
		return
	}
	for _, c := range cols {
		if c.ID != req.AlbumID {
			continue
		}
		t := lrlink.Target{AlbumID: c.ID, Name: c.Name, Path: c.Path, SetAt: a.lr.now().UTC()}
		if err := a.lr.link.PutTarget(r.Context(), t); err != nil {
			a.fail(w, r, err)
			return
		}
		slog.InfoContext(r.Context(), "lightroom target set", "album_id", c.ID, "path", c.Path)
		writeJSON(w, t)
		return
	}
	http.Error(w, "no collection with that id", http.StatusNotFound)
}
