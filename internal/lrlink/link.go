// Package lrlink is how the gateway tells the publisher where photos go in
// Adobe Lightroom and with which access token. Both live in the JetStream
// key-value bucket "keeper-lightroom":
//
//	target  JSON Target: the collection the user picked in the console
//	token   AES-GCM sealed JSON Token: the current access token only
//
// The gateway owns the OAuth grant (the refresh token stays sealed in its
// Postgres schema), refreshes the access token before it expires and puts
// each new one here. The publisher watches the bucket, so a new target or
// token takes effect on the next attempt without a restart, and it never
// holds a refresh token.
package lrlink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/geilername/keeper/internal/lightroom"
)

// Bucket and keys.
const (
	Bucket    = "keeper-lightroom"
	KeyTarget = "target"
	KeyToken  = "token"
)

// tokenAAD binds a sealed KV token to this bucket and key.
var tokenAAD = []byte("keeper/" + Bucket + "/" + KeyToken + "/v1")

// Target is the collection photos are added to.
type Target struct {
	AlbumID string    `json:"album_id"`
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	SetAt   time.Time `json:"set_at"`
}

// Token is an access token as the publisher sees it.
type Token struct {
	AccessToken string    `json:"access_token"`
	ExpiresAt   time.Time `json:"expires_at"`
	AccountID   string    `json:"account_id"`
}

// OpenBucket binds to the bucket, creating it if it is missing (one value
// per key, file storage: it survives a NATS restart).
func OpenBucket(ctx context.Context, js jetstream.JetStream) (jetstream.KeyValue, error) {
	kv, err := js.KeyValue(ctx, Bucket)
	if err == nil {
		return kv, nil
	}
	if !errors.Is(err, jetstream.ErrBucketNotFound) {
		return nil, err
	}
	kv, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:      Bucket,
		Description: "Keeper: Lightroom target collection and sealed access token",
		History:     1,
		Storage:     jetstream.FileStorage,
	})
	if errors.Is(err, jetstream.ErrBucketExists) {
		return js.KeyValue(ctx, Bucket) // another service won the race
	}
	return kv, err
}

// Writer is the gateway's side of the bucket.
type Writer struct {
	KV     jetstream.KeyValue
	Sealer *Sealer
}

// PutToken seals and stores the current access token.
func (w Writer) PutToken(ctx context.Context, t Token) error {
	plain, err := json.Marshal(t)
	if err != nil {
		return err
	}
	_, err = w.KV.Put(ctx, KeyToken, w.Sealer.Seal(plain, tokenAAD))
	return err
}

// DeleteToken removes the token (disconnect, or the grant was revoked).
func (w Writer) DeleteToken(ctx context.Context) error {
	err := w.KV.Delete(ctx, KeyToken)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil
	}
	return err
}

// PutTarget stores the chosen collection.
func (w Writer) PutTarget(ctx context.Context, t Target) error {
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	_, err = w.KV.Put(ctx, KeyTarget, data)
	return err
}

// Target reads the chosen collection; ok is false when none is chosen.
func (w Writer) Target(ctx context.Context) (Target, bool, error) {
	e, err := w.KV.Get(ctx, KeyTarget)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return Target{}, false, nil
	}
	if err != nil {
		return Target{}, false, err
	}
	var t Target
	if err := json.Unmarshal(e.Value(), &t); err != nil {
		return Target{}, false, err
	}
	return t, t.AlbumID != "", nil
}

// Link is the publisher's view of the bucket: the latest target and token,
// kept current by Watch. It is a lightroom.TokenSource.
type Link struct {
	sealer *Sealer
	now    func() time.Time

	mu        sync.Mutex
	target    *Target
	token     *Token
	onAccount func()
}

var _ lightroom.TokenSource = (*Link)(nil)

// NewLink returns an empty Link; Watch or Apply fill it.
func NewLink(s *Sealer) *Link { return &Link{sealer: s, now: time.Now} }

// OnAccountChange calls f whenever the token belongs to a different Adobe
// account than before (the client drops its cached catalogue).
func (l *Link) OnAccountChange(f func()) { l.mu.Lock(); l.onAccount = f; l.mu.Unlock() }

// Apply takes one bucket update. A value that does not decode is dropped
// (and reported) rather than kept half-applied.
func (l *Link) Apply(key string, value []byte, deleted bool) error {
	switch key {
	case KeyTarget:
		var t *Target
		if !deleted {
			t = new(Target)
			if err := json.Unmarshal(value, t); err != nil {
				return fmt.Errorf("lrlink: target: %w", err)
			}
			if t.AlbumID == "" {
				t = nil
			}
		}
		l.mu.Lock()
		l.target = t
		l.mu.Unlock()
	case KeyToken:
		var t *Token
		if !deleted {
			plain, err := l.sealer.Open(value, tokenAAD)
			if err != nil {
				l.setToken(nil)
				return fmt.Errorf("lrlink: token: %w", err)
			}
			t = new(Token)
			if err := json.Unmarshal(plain, t); err != nil {
				l.setToken(nil)
				return fmt.Errorf("lrlink: token: %w", err)
			}
		}
		l.setToken(t)
	}
	return nil
}

func (l *Link) setToken(t *Token) {
	l.mu.Lock()
	changed := t != nil && (l.token == nil || l.token.AccountID != t.AccountID)
	l.token = t
	f := l.onAccount
	l.mu.Unlock()
	if changed && f != nil {
		f()
	}
}

// Watch applies every update of the bucket until ctx ends.
func (l *Link) Watch(ctx context.Context, kv jetstream.KeyValue) error {
	w, err := kv.WatchAll(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = w.Stop() }()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e, ok := <-w.Updates():
			if !ok {
				return errors.New("lrlink: watcher closed")
			}
			if e == nil {
				continue // end of the initial values
			}
			deleted := e.Operation() != jetstream.KeyValuePut
			if err := l.Apply(e.Key(), e.Value(), deleted); err != nil {
				slog.WarnContext(ctx, "ignoring Lightroom link update", "key", e.Key(), "err", err)
			}
		}
	}
}

// Target returns the chosen collection, if any.
func (l *Link) Target() (Target, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.target == nil {
		return Target{}, false
	}
	return *l.target, true
}

// expirySkew treats a token this close to its expiry as expired: a call
// started now must not reach Adobe with a dead token.
const expirySkew = 30 * time.Second

// AccessToken returns the current access token, or
// lightroom.ErrNotConnected when there is none or it has expired (the
// gateway refreshes well before that, so this means the grant is gone).
func (l *Link) AccessToken(context.Context) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.token == nil || l.token.AccessToken == "" || !l.now().Add(expirySkew).Before(l.token.ExpiresAt) {
		return "", lightroom.ErrNotConnected
	}
	return l.token.AccessToken, nil
}
