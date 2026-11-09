package lightroom_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/geilername/keeper/internal/lightroom"
	"github.com/geilername/keeper/internal/lightroom/lightroomtest"
)

func setup(t *testing.T, timeout time.Duration) (*lightroomtest.Server, *lightroom.HTTP) {
	t.Helper()
	srv := lightroomtest.NewServer(lightroom.DefaultCatalogID)
	t.Cleanup(srv.Close)
	return srv, lightroom.NewHTTP(srv.URL+"/", lightroom.DefaultCatalogID, timeout)
}

func photo(content string) ([]byte, string) {
	b := []byte(content)
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:])
}

func TestAssetID(t *testing.T) {
	_, sha := photo("x")
	if got := lightroom.AssetID(sha); got != sha[:32] {
		t.Fatalf("AssetID = %q, want first 32 hex chars", got)
	}
}

func TestIdempotentRepeat(t *testing.T) {
	srv, c := setup(t, time.Second)
	ctx := context.Background()
	body, sha := photo("jpeg bytes")
	id := lightroom.AssetID(sha)

	for i, wantCreated := range []bool{true, false} {
		a, created, err := c.CreateAsset(ctx, lightroom.NewAsset{ID: id, FileName: "a.jpg", SHA256: sha})
		if err != nil {
			t.Fatalf("create #%d: %v", i, err)
		}
		if created != wantCreated || a.ID != id || a.SHA256 != sha {
			t.Fatalf("create #%d = %+v created=%v, want created=%v", i, a, created, wantCreated)
		}
		up, err := c.UploadMaster(ctx, id, "image/jpeg", strings.NewReader(string(body)), int64(len(body)))
		if err != nil || up != wantCreated {
			t.Fatalf("upload #%d: created=%v err=%v", i, up, err)
		}
		if err := c.SetRating(ctx, id, 5); err != nil {
			t.Fatalf("rating #%d: %v", i, err)
		}
	}

	// After the master is up, create reports its sha256 so callers can skip.
	a, _, err := c.CreateAsset(ctx, lightroom.NewAsset{ID: id, FileName: "a.jpg", SHA256: sha})
	if err != nil || a.MasterSHA256 != sha {
		t.Fatalf("create after upload: %+v, %v", a, err)
	}
	if got, _ := srv.Get(id); srv.Len() != 1 || got.Rating != 5 || got.MasterSHA256 != sha {
		t.Fatalf("store = %d assets, %+v", srv.Len(), got)
	}
}

func TestClassify(t *testing.T) {
	ctx := context.Background()
	_, sha := photo("y")
	id := lightroom.AssetID(sha)
	req := lightroom.NewAsset{ID: id, FileName: "y.jpg", SHA256: sha}

	t.Run("503 is retryable", func(t *testing.T) {
		srv, c := setup(t, time.Second)
		srv.Fail(http.StatusServiceUnavailable)
		_, _, err := c.CreateAsset(ctx, req)
		var se *lightroom.StatusError
		if !errors.As(err, &se) || se.StatusCode != 503 {
			t.Fatalf("err = %v, want StatusError 503", err)
		}
		assertClass(t, err, lightroom.ClassServer, true)
	})

	t.Run("409 conflict is permanent", func(t *testing.T) {
		srv, c := setup(t, time.Second)
		_, other := photo("different bytes")
		srv.Put(id, lightroomtest.Asset{SHA256: other})
		_, _, err := c.CreateAsset(ctx, req)
		assertClass(t, err, lightroom.ClassConflict, false)
	})

	t.Run("400 is permanent", func(t *testing.T) {
		srv, c := setup(t, time.Second)
		srv.Put(id, lightroomtest.Asset{SHA256: sha})
		assertClass(t, c.SetRating(ctx, id, 9), lightroom.ClassClient, false)
	})

	t.Run("slow server is a timeout", func(t *testing.T) {
		srv, c := setup(t, 50*time.Millisecond)
		srv.Delay(time.Second)
		_, _, err := c.CreateAsset(ctx, req)
		assertClass(t, err, lightroom.ClassTimeout, true)
	})

	t.Run("connection refused is unreachable", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := l.Addr().String()
		l.Close() // nothing listens there now
		c := lightroom.NewHTTP("http://"+addr, lightroom.DefaultCatalogID, time.Second)
		_, _, err = c.CreateAsset(ctx, req)
		assertClass(t, err, lightroom.ClassUnreachable, true)
	})

	t.Run("canceled context", func(t *testing.T) {
		_, c := setup(t, time.Second)
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		_, _, err := c.CreateAsset(cctx, req)
		assertClass(t, err, lightroom.ClassCanceled, true)
	})
}

func assertClass(t *testing.T, err error, wantClass string, wantRetry bool) {
	t.Helper()
	if err == nil {
		t.Fatal("err = nil")
	}
	class, retry := lightroom.Classify(err)
	if class != wantClass || retry != wantRetry {
		t.Fatalf("Classify(%v) = %s,%v; want %s,%v", err, class, retry, wantClass, wantRetry)
	}
}
