package lrlink

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/geilername/keeper/internal/lightroom"
)

func key(b byte) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32)) }

func TestSealer(t *testing.T) {
	s, err := NewSealer(key(1))
	if err != nil {
		t.Fatal(err)
	}
	sealed := s.Seal([]byte("secret token"), []byte("here"))
	if bytes.Contains(sealed, []byte("secret")) {
		t.Fatal("plaintext visible in sealed value")
	}
	if again := s.Seal([]byte("secret token"), []byte("here")); bytes.Equal(again, sealed) {
		t.Fatal("same nonce twice")
	}
	if plain, err := s.Open(sealed, []byte("here")); err != nil || string(plain) != "secret token" {
		t.Fatalf("open: %q %v", plain, err)
	}
	if _, err := s.Open(sealed, []byte("elsewhere")); !errors.Is(err, ErrOpen) {
		t.Fatalf("other aad: %v", err)
	}
	other, _ := NewSealer(key(2))
	if _, err := other.Open(sealed, []byte("here")); !errors.Is(err, ErrOpen) {
		t.Fatalf("other key: %v", err)
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := s.Open(sealed, []byte("here")); !errors.Is(err, ErrOpen) {
		t.Fatalf("tampered: %v", err)
	}
	if _, err := s.Open([]byte("x"), nil); !errors.Is(err, ErrOpen) {
		t.Fatalf("short: %v", err)
	}
}

func TestNewSealerRejectsBadKeys(t *testing.T) {
	if _, err := NewSealer(""); !errors.Is(err, ErrNoKey) {
		t.Fatalf("empty: %v", err)
	}
	if _, err := NewSealer(base64.StdEncoding.EncodeToString(make([]byte, 16))); err == nil {
		t.Fatal("16-byte key accepted")
	}
	if _, err := NewSealer("not base64 !!"); err == nil {
		t.Fatal("garbage accepted")
	}
	if _, err := NewSealer(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, 32))); err != nil {
		t.Fatalf("url-safe key: %v", err)
	}
}

func sealedToken(t *testing.T, s *Sealer, tok Token) []byte {
	t.Helper()
	b, err := json.Marshal(tok)
	if err != nil {
		t.Fatal(err)
	}
	return s.Seal(b, tokenAAD)
}

func TestLinkApply(t *testing.T) {
	s, _ := NewSealer(key(1))
	l := NewLink(s)
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }
	resets := 0
	l.OnAccountChange(func() { resets++ })
	ctx := context.Background()

	if _, ok := l.Target(); ok {
		t.Fatal("target before any update")
	}
	if _, err := l.AccessToken(ctx); !errors.Is(err, lightroom.ErrNotConnected) {
		t.Fatalf("token before any update: %v", err)
	}

	if err := l.Apply(KeyTarget, []byte(`{"album_id":"col1","name":"Talks","path":"2026 / Talks"}`), false); err != nil {
		t.Fatal(err)
	}
	if tg, ok := l.Target(); !ok || tg.AlbumID != "col1" || tg.Name != "Talks" {
		t.Fatalf("target %+v", tg)
	}
	if err := l.Apply(KeyToken, sealedToken(t, s, Token{AccessToken: "a1", ExpiresAt: now.Add(time.Hour), AccountID: "u1"}), false); err != nil {
		t.Fatal(err)
	}
	if tok, err := l.AccessToken(ctx); err != nil || tok != "a1" {
		t.Fatalf("token %q %v", tok, err)
	}
	// Same account: no reset. Another account: the client forgets its catalogue.
	_ = l.Apply(KeyToken, sealedToken(t, s, Token{AccessToken: "a2", ExpiresAt: now.Add(time.Hour), AccountID: "u1"}), false)
	_ = l.Apply(KeyToken, sealedToken(t, s, Token{AccessToken: "a3", ExpiresAt: now.Add(time.Hour), AccountID: "u2"}), false)
	if resets != 2 { // first token, then u2
		t.Fatalf("account resets = %d, want 2", resets)
	}

	// Within 30 s of expiry the token is not handed out.
	now = now.Add(time.Hour - 10*time.Second)
	if _, err := l.AccessToken(ctx); !errors.Is(err, lightroom.ErrNotConnected) {
		t.Fatalf("expiring token: %v", err)
	}

	// A token sealed with another key is dropped, not kept.
	other, _ := NewSealer(key(2))
	if err := l.Apply(KeyToken, sealedToken(t, other, Token{AccessToken: "x", ExpiresAt: now.Add(time.Hour)}), false); err == nil {
		t.Fatal("foreign token accepted")
	}
	if _, err := l.AccessToken(ctx); !errors.Is(err, lightroom.ErrNotConnected) {
		t.Fatalf("after foreign token: %v", err)
	}

	if err := l.Apply(KeyTarget, nil, true); err != nil {
		t.Fatal(err)
	}
	if _, ok := l.Target(); ok {
		t.Fatal("target after delete")
	}
}

// Integration against a throwaway JetStream:
//
//	docker run -d --rm --name lrlink-nats -p 14222:4222 nats:2.15.2-alpine -js
//	NATS_URL=nats://127.0.0.1:14222 go test -race -run Integration ./internal/lrlink/
func TestIntegrationWriterAndWatch(t *testing.T) {
	url := os.Getenv("NATS_URL")
	if url == "" {
		t.Skip("NATS_URL not set")
	}
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, _ := jetstream.New(nc)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	kv, err := OpenBucket(ctx, js)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenBucket(ctx, js); err != nil { // second open binds
		t.Fatal(err)
	}
	s, _ := NewSealer(key(7))
	w := Writer{KV: kv, Sealer: s}
	l := NewLink(s)
	go func() { _ = l.Watch(ctx, kv) }()

	exp := time.Now().Add(time.Hour).UTC()
	if err := w.PutTarget(ctx, Target{AlbumID: "col9", Name: "Talks"}); err != nil {
		t.Fatal(err)
	}
	if err := w.PutToken(ctx, Token{AccessToken: "tok9", ExpiresAt: exp}); err != nil {
		t.Fatal(err)
	}
	e, _ := kv.Get(ctx, KeyToken)
	if bytes.Contains(e.Value(), []byte("tok9")) {
		t.Fatal("token stored in the clear")
	}
	for ctx.Err() == nil {
		tg, ok := l.Target()
		tok, err := l.AccessToken(ctx)
		if ok && tg.AlbumID == "col9" && err == nil && tok == "tok9" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ctx.Err() != nil {
		t.Fatal("watch never delivered target and token")
	}
	if tg, ok, err := w.Target(ctx); err != nil || !ok || tg.AlbumID != "col9" {
		t.Fatalf("writer target %+v %v %v", tg, ok, err)
	}
	_ = w.DeleteToken(ctx)
}
