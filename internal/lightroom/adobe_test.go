package lightroom_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/geilername/keeper/internal/lightroom"
	"github.com/geilername/keeper/internal/lightroom/lightroomtest"
)

const (
	testKey   = "keeper-test-api-key"
	testToken = "token-1"
)

func adobe(t *testing.T) (*lightroomtest.Adobe, *lightroom.Adobe) {
	t.Helper()
	srv := lightroomtest.NewAdobe(testKey, testToken)
	t.Cleanup(srv.Close)
	return srv, lightroom.NewAdobe(srv.URL, testKey, lightroom.StaticToken(testToken), time.Second)
}

func TestAdobeCreateUploadIsIdempotent(t *testing.T) {
	srv, c := adobe(t)
	ctx := context.Background()
	body, sha := photo("raw bytes")
	id := lightroom.AssetID(sha)

	a, created, err := c.CreateAsset(ctx, lightroom.NewAsset{ID: id, FileName: "DSC01234.ARW", SHA256: sha})
	if err != nil || !created || a.ID != id {
		t.Fatalf("create: %+v created=%v err=%v", a, created, err)
	}
	got, _ := srv.Asset(id)
	if got.ImportedOnDevice != testKey || got.ImportedBy != srv.AccountID || got.FileName != "DSC01234.ARW" {
		t.Fatalf("import source = %+v, want the API key as device and the account as importer", got)
	}
	// A raw goes up as application/octet-stream (the spec allows only
	// image/jpeg for JPEGs).
	if created, err := c.UploadMaster(ctx, id, "image/x-sony-arw", bytes.NewReader(body), int64(len(body))); err != nil || !created {
		t.Fatalf("upload: created=%v err=%v", created, err)
	}
	if got, _ := srv.Asset(id); got.MasterType != "application/octet-stream" || got.MasterSHA256 != sha {
		t.Fatalf("master = %+v", got)
	}

	// Replay: create answers 403 "already exists", the client reads the
	// asset back and sees the master by its sha256.
	a, created, err = c.CreateAsset(ctx, lightroom.NewAsset{ID: id, FileName: "DSC01234.ARW", SHA256: sha})
	if err != nil || created || a.MasterSHA256 != sha {
		t.Fatalf("replayed create: %+v created=%v err=%v", a, created, err)
	}
	if created, err := c.UploadMaster(ctx, id, "image/jpeg", bytes.NewReader(body), int64(len(body))); err != nil || created {
		t.Fatalf("replayed upload: created=%v err=%v", created, err)
	}
	if n := srv.Calls("create_asset", http.StatusForbidden); n != 1 {
		t.Fatalf("create 403s = %d", n)
	}
	// Account and catalogue were looked up once and cached.
	if srv.Calls("get_account", 200) != 1 || srv.Calls("get_catalog", 200) != 1 {
		t.Fatalf("account %d, catalog %d lookups", srv.Calls("get_account", 200), srv.Calls("get_catalog", 200))
	}
}

func TestAdobeRatingIsUnsupported(t *testing.T) {
	_, c := adobe(t)
	if err := c.SetRating(context.Background(), "x", 5); !errors.Is(err, lightroom.ErrRatingUnsupported) {
		t.Fatalf("SetRating = %v", err)
	}
}

func TestStripsWhilePrefix(t *testing.T) {
	for _, prefix := range []string{"while(1){}", "while (1) {}\n", "  while( 1 ){ }\r\n", ""} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, prefix+`{"id":"cat1"}`)
		}))
		c := lightroom.NewAdobe(srv.URL, testKey, lightroom.StaticToken("t"), time.Second)
		id, err := c.Catalog(context.Background())
		srv.Close()
		if err != nil || id != "cat1" {
			t.Fatalf("prefix %q: id=%q err=%v", prefix, id, err)
		}
	}
}

func TestAdobeErrorsClassify(t *testing.T) {
	srv, c := adobe(t)
	ctx := context.Background()

	// An expired token is 403 with code 4300 behind the prefix: retryable.
	srv.SetToken("token-2")
	_, err := c.Catalog(ctx)
	if class, retry := lightroom.Classify(err); class != lightroom.ClassAuth || !retry {
		t.Fatalf("expired token: %v → %s retry=%v", err, class, retry)
	}
	// No token at all never reaches Adobe.
	none := lightroom.NewAdobe(srv.URL, testKey, lightroom.StaticToken(""), time.Second)
	_, err = none.Catalog(ctx)
	if !errors.Is(err, lightroom.ErrNotConnected) {
		t.Fatalf("no token: %v", err)
	}
	// A wrong API key is permanent.
	bad := lightroom.NewAdobe(srv.URL, "wrong", lightroom.StaticToken("token-2"), time.Second)
	_, err = bad.Catalog(ctx)
	var se *lightroom.StatusError
	if class, retry := lightroom.Classify(err); class != lightroom.ClassClient || retry || !errors.As(err, &se) || se.Code != "403003" {
		t.Fatalf("bad key: %v → %s retry=%v", err, class, retry)
	}
	srv.SetToken(testToken)
	srv.Fail(http.StatusServiceUnavailable)
	_, err = c.Catalog(ctx)
	if class, retry := lightroom.Classify(err); class != lightroom.ClassServer || !retry {
		t.Fatalf("503: %s %v", class, retry)
	}
}

func TestAdobeCatalogChange(t *testing.T) {
	srv, c := adobe(t)
	ctx := context.Background()
	_, sha := photo("one")
	if _, _, err := c.CreateAsset(ctx, lightroom.NewAsset{ID: lightroom.AssetID(sha), FileName: "a.jpg", SHA256: sha}); err != nil {
		t.Fatal(err)
	}
	// The user's catalogue ID changed (quickstart/upload.md): 404, retryable,
	// and the next call looks the catalogue up again.
	srv.SetCatalogID("d00dd00dd00dd00dd00dd00dd00dd00d")
	_, sha2 := photo("two")
	_, _, err := c.CreateAsset(ctx, lightroom.NewAsset{ID: lightroom.AssetID(sha2), FileName: "b.jpg", SHA256: sha2})
	if class, retry := lightroom.Classify(err); class != lightroom.ClassCatalog || !retry {
		t.Fatalf("catalog gone: %v → %s %v", err, class, retry)
	}
	if _, _, err := c.CreateAsset(ctx, lightroom.NewAsset{ID: lightroom.AssetID(sha2), FileName: "b.jpg", SHA256: sha2}); err != nil {
		t.Fatalf("after catalog change: %v", err)
	}
}

func TestListCollectionsPagesAndNests(t *testing.T) {
	srv, c := adobe(t)
	srv.PageSize = 2
	for _, a := range []lightroomtest.Album{
		{ID: "set1", Name: "2026", Subtype: "collection_set"},
		{ID: "set2", Name: "Conferences", Subtype: "collection_set", ParentID: "set1"},
		{ID: "set3", Name: "Family", Subtype: "collection_set"},
		{ID: "col1", Name: "KubeCon", Subtype: "collection", ParentID: "set2"},
		{ID: "col2", Name: "Talks", Subtype: "collection"},
		{ID: "col3", Name: "Birthday", Subtype: "collection", ParentID: "set3"},
		{ID: "col4", Name: "Archive", Subtype: "collection"},
		{ID: "col5", Name: "Orphan", Subtype: "collection", ParentID: "gone"},
		{ID: "prj1", Name: "Partner project", Subtype: "project"},
	} {
		srv.AddAlbum(a)
	}
	cols, err := c.ListCollections(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range cols {
		got = append(got, c.ID+"="+c.Path)
	}
	want := "col1=2026 / Conferences / KubeCon,col4=Archive,col3=Family / Birthday,col5=Orphan,col2=Talks"
	if strings.Join(got, ",") != want {
		t.Fatalf("collections:\n got %s\nwant %s", strings.Join(got, ","), want)
	}
	// 5 collections at 2 per page = 3 pages; 3 sets = 2 pages.
	if n := srv.Calls("list_albums", 200); n != 5 {
		t.Fatalf("album pages fetched = %d, want 5", n)
	}
}

func TestListCollectionsStaysOnHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/catalog" {
			fmt.Fprint(w, `while(1){}{"id":"cat"}`)
			return
		}
		fmt.Fprint(w, `while(1){}{"base":"https://evil.example/v2/","resources":[],"links":{"next":{"href":"albums?x=1"}}}`)
	}))
	defer srv.Close()
	c := lightroom.NewAdobe(srv.URL, testKey, lightroom.StaticToken("t"), time.Second)
	if _, err := c.ListCollections(context.Background()); err == nil || !strings.Contains(err.Error(), "leaves") {
		t.Fatalf("err = %v, want refusal to follow a link off the API host", err)
	}
}

func TestAddToAlbum(t *testing.T) {
	srv, c := adobe(t)
	ctx := context.Background()
	srv.AddAlbum(lightroomtest.Album{ID: "col1", Name: "Talks", Subtype: "collection"})
	var ids []string
	for i := range 3 {
		_, sha := photo(fmt.Sprint("p", i))
		id := lightroom.AssetID(sha)
		if _, _, err := c.CreateAsset(ctx, lightroom.NewAsset{ID: id, FileName: "x.jpg", SHA256: sha}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	if err := c.AddToAlbum(ctx, "col1", ids[:1]); err != nil {
		t.Fatal(err)
	}
	// One already in, two new: 201 with a per-asset error for the first.
	if err := c.AddToAlbum(ctx, "col1", ids); err != nil {
		t.Fatalf("mixed add: %v", err)
	}
	// All already in: 403, but every asset is in the album, so success.
	if err := c.AddToAlbum(ctx, "col1", ids); err != nil {
		t.Fatalf("replayed add: %v", err)
	}
	for _, id := range ids {
		if !srv.InAlbum("col1", id) {
			t.Fatalf("%s not in album", id)
		}
	}
	if srv.Calls("add_to_album", 201) != 2 || srv.Calls("add_to_album", 403) != 1 {
		t.Fatalf("adds: %d×201 %d×403", srv.Calls("add_to_album", 201), srv.Calls("add_to_album", 403))
	}
}

func TestAddToAlbumBatchesOf50(t *testing.T) {
	srv, c := adobe(t)
	ctx := context.Background()
	srv.AddAlbum(lightroomtest.Album{ID: "col1", Name: "Talks", Subtype: "collection"})
	var ids []string
	for i := range 120 {
		_, sha := photo(fmt.Sprint("b", i))
		id := lightroom.AssetID(sha)
		if _, _, err := c.CreateAsset(ctx, lightroom.NewAsset{ID: id, FileName: "x.jpg", SHA256: sha}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := c.AddToAlbum(ctx, "col1", ids); err != nil {
		t.Fatal(err)
	}
	if n := srv.Calls("add_to_album", 201); n != 3 {
		t.Fatalf("add calls = %d, want 3 (50+50+20)", n)
	}
}

func TestAddToAlbumRefused(t *testing.T) {
	srv, c := adobe(t)
	ctx := context.Background()
	srv.AddAlbum(lightroomtest.Album{ID: "col1", Name: "Talks", Subtype: "collection"})
	srv.Refuse("col1")
	_, sha := photo("refused")
	id := lightroom.AssetID(sha)
	if _, _, err := c.CreateAsset(ctx, lightroom.NewAsset{ID: id, FileName: "x.jpg", SHA256: sha}); err != nil {
		t.Fatal(err)
	}
	err := c.AddToAlbum(ctx, "col1", []string{id})
	var refused *lightroom.AlbumRefusedError
	if !errors.As(err, &refused) || len(refused.AssetIDs) != 1 || refused.StatusCode != 403 {
		t.Fatalf("err = %v, want AlbumRefusedError", err)
	}
	if class, retry := lightroom.Classify(err); class != lightroom.ClassRefused || retry {
		t.Fatalf("class %s retry %v", class, retry)
	}
	// A collection that no longer exists is refused too.
	err = c.AddToAlbum(ctx, "deleted", []string{id})
	if !errors.As(err, &refused) || refused.Description != "collection not found" {
		t.Fatalf("deleted collection: %v", err)
	}
	// An expired token on the add is not a refusal.
	srv.SetToken("token-2")
	err = c.AddToAlbum(ctx, "col1", []string{id})
	if class, _ := lightroom.Classify(err); class != lightroom.ClassAuth {
		t.Fatalf("expired token on add: %v → %s", err, class)
	}
}

func TestIMSExchangeAndRefresh(t *testing.T) {
	ims := lightroomtest.NewIMS("cid", "secret", "the-code")
	defer ims.Close()
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	c := &lightroom.IMS{BaseURL: ims.URL, ClientID: "cid", ClientSecret: "secret",
		RedirectURI: "https://localhost:8443/auth/lightroom/callback", Now: func() time.Time { return now }}

	u, err := url.Parse(c.AuthURL("st4te"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Path != "/ims/authorize/v2" || q.Get("client_id") != "cid" || q.Get("state") != "st4te" ||
		q.Get("response_type") != "code" || q.Get("scope") != "openid,AdobeID,lr_partner_apis,offline_access" ||
		q.Get("redirect_uri") != "https://localhost:8443/auth/lightroom/callback" {
		t.Fatalf("auth url %s", u)
	}

	ts, err := c.Exchange(context.Background(), "the-code")
	if err != nil {
		t.Fatal(err)
	}
	if ts.AccessToken != "access-1" || ts.RefreshToken != "refresh-1" || !ts.ExpiresAt.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("exchange: %+v", ts)
	}
	if _, err := c.Exchange(context.Background(), "the-code"); err == nil {
		t.Fatal("a code works once")
	}
	ts, err = c.Refresh(context.Background(), ts.RefreshToken)
	if err != nil || ts.AccessToken != "access-2" || ts.RefreshToken != "refresh-2" {
		t.Fatalf("refresh: %+v %v", ts, err)
	}
	ims.Revoke()
	_, err = c.Refresh(context.Background(), ts.RefreshToken)
	var ie *lightroom.IMSError
	if !errors.As(err, &ie) || !ie.Revoked() {
		t.Fatalf("revoked refresh: %v", err)
	}
	bad := *c
	bad.ClientSecret = "nope"
	if _, err := bad.Refresh(context.Background(), "x"); !errors.As(err, &ie) || ie.Code != "invalid_client" {
		t.Fatalf("bad secret: %v", err)
	}
}

func TestIMSExpiresInMilliseconds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"access_token":"a","expires_in":86399000}`) // token/v1 style
	}))
	defer srv.Close()
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	c := &lightroom.IMS{BaseURL: srv.URL, Now: func() time.Time { return now }}
	ts, err := c.Refresh(context.Background(), "r")
	if err != nil || !ts.ExpiresAt.Equal(now.Add(86399*time.Second)) || ts.RefreshToken != "r" {
		t.Fatalf("%+v %v", ts, err)
	}
}
