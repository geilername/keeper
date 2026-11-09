package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/geilername/keeper/internal/events"
	"github.com/geilername/keeper/internal/lightroom"
	"github.com/geilername/keeper/internal/lrlink"
)

const xmpLen = 4096

// sonyRaw is a minimal ARW-shaped TIFF: IFD0 holds a camera XMP packet
// (4096 bytes, rating 2, space padding) followed by "raw data".
func sonyRaw(t *testing.T, tag string) (file []byte, xmpOff int) {
	t.Helper()
	head := "<?xpacket begin='\xef\xbb\xbf' id='W5M0MpCehiHzreSzNTczkc9d'?>\n" +
		"<x:xmpmeta xmlns:x='adobe:ns:meta/' x:xmptk=''>\n" +
		"<rdf:RDF xmlns:rdf='http://www.w3.org/1999/02/22-rdf-syntax-ns#'>\n" +
		" <rdf:Description rdf:about=''\n  xmlns:xmp='http://ns.adobe.com/xap/1.0/'>\n" +
		"  <xmp:Rating>2</xmp:Rating>\n </rdf:Description>\n</rdf:RDF>\n</x:xmpmeta>\n"
	trailer := "<?xpacket end='w'?>"
	packet := head + strings.Repeat(" ", xmpLen-len(head)-len(trailer)) + trailer

	bo := binary.LittleEndian
	b := []byte("II*\x00\x08\x00\x00\x00")
	ifd := make([]byte, 2+12+4)
	bo.PutUint16(ifd, 1)
	xmpOff = 8 + len(ifd)
	bo.PutUint16(ifd[2:], 0x02BC)
	bo.PutUint16(ifd[4:], 1)
	bo.PutUint32(ifd[6:], xmpLen)
	bo.PutUint32(ifd[10:], uint32(xmpOff))
	b = append(b, ifd...)
	b = append(b, packet...)
	b = append(b, bytes.Repeat([]byte("sensor data "+tag+" "), 500)...)
	return b, xmpOff
}

// rawPhoto writes an ARW original to the spool and returns its decision.
func rawPhoto(t *testing.T, content []byte, rating int) events.PhotoDecided {
	t.Helper()
	sum := sha256.Sum256(content)
	sha := hex.EncodeToString(sum[:])
	path := filepath.Join(os.Getenv("SPOOL_DIR"), "photos", sha+".arw")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return events.PhotoDecided{V: 1, SHA256: sha, FileName: "DSC07625.ARW", Bucket: events.BucketBest, Rating: rating}
}

func spanBool(h *harness, key string) (val, found bool) {
	for _, s := range h.spans.Ended() {
		for _, kv := range s.Attributes() {
			if string(kv.Key) == key {
				val, found = kv.Value.AsBool(), true
			}
		}
	}
	return val, found
}

func TestMockRawGetsRatingEmbedded(t *testing.T) {
	h := newHarness(t)
	raw, xmpOff := sonyRaw(t, "mock")
	ev := rawPhoto(t, raw, 5)
	id := lightroom.AssetID(ev.SHA256)

	for delivered := 1; delivered <= 2; delivered++ {
		if permanent, err := h.p.attempt(context.Background(), delivered, ev); err != nil || permanent {
			t.Fatalf("delivery %d: permanent=%v err=%v", delivered, permanent, err)
		}
	}
	a, ok := h.lr.Get(id)
	if !ok || h.lr.Len() != 1 {
		t.Fatalf("asset %s missing (%d assets)", id, h.lr.Len())
	}
	// Lightroom got a different file than the one shot: same size, rated.
	if a.MasterSHA256 == ev.SHA256 || a.SHA256 != a.MasterSHA256 || a.MasterType != "image/x-sony-arw" {
		t.Fatalf("asset = %+v; want importSource.sha256 = master sha256 of the rated copy", a)
	}
	// Redelivery made the same bytes and skipped the upload.
	if h.lr.Calls("upload_master", 201) != 1 || h.lr.Calls("upload_master", 200) != 0 {
		t.Fatalf("uploads 201×%d 200×%d", h.lr.Calls("upload_master", 201), h.lr.Calls("upload_master", 200))
	}
	if v, ok := spanBool(h, "keeper.rating.embedded"); !ok || !v {
		t.Fatal("publish span does not say the rating was embedded")
	}
	// The derived copy is gone once published.
	if left, _ := filepath.Glob(filepath.Join(h.p.ratedDir, "*")); len(left) != 0 {
		t.Fatalf("rated copies left behind: %v", left)
	}

	// Rebuild the copy to check its bytes: only the packet differs.
	m, err := h.p.ratedCopy(filepath.Join(os.Getenv("SPOOL_DIR"), "photos", ev.SHA256+".arw"), ev)
	if err != nil {
		t.Fatal(err)
	}
	defer m.f.Close()
	if m.sha != a.MasterSHA256 {
		t.Fatal("rated copy is not deterministic")
	}
	got := make([]byte, m.size)
	if _, err := m.f.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(raw) || !bytes.Equal(got[:xmpOff], raw[:xmpOff]) || !bytes.Equal(got[xmpOff+xmpLen:], raw[xmpOff+xmpLen:]) {
		t.Fatal("bytes outside the XMP packet changed")
	}
	pkt := string(got[xmpOff : xmpOff+xmpLen])
	if !strings.Contains(pkt, "<xmp:Rating>5</xmp:Rating>") || !strings.Contains(pkt, "<keeper:CameraRating>2</keeper:CameraRating>") {
		t.Fatalf("packet:\n%s", pkt)
	}
}

func TestAdobeRawGetsRatingEmbedded(t *testing.T) {
	h := newAdobeHarness(t)
	h.targets.set(lrlink.Target{AlbumID: "col1", Name: "Talks"})
	raw, _ := sonyRaw(t, "adobe")
	ev := rawPhoto(t, raw, 1)
	id := lightroom.AssetID(ev.SHA256)

	for delivered := 1; delivered <= 2; delivered++ {
		if _, err := h.p.attempt(context.Background(), delivered, ev); err != nil {
			t.Fatalf("delivery %d: %v", delivered, err)
		}
	}
	a, _ := h.adobe.Asset(id)
	if a.MasterType != "application/octet-stream" || a.MasterSHA256 == ev.SHA256 || a.MasterSHA256 == "" {
		t.Fatalf("asset %+v", a)
	}
	// The replay saw the rated copy's sha256 via GET and did not upload again.
	if h.adobe.Calls("upload_master", 201) != 1 || h.adobe.Calls("upload_master", 403) != 0 {
		t.Fatalf("uploads 201×%d 403×%d", h.adobe.Calls("upload_master", 201), h.adobe.Calls("upload_master", 403))
	}
	if !h.adobe.InAlbum("col1", id) {
		t.Fatal("not in the collection")
	}
}

// A raw whose XMP cannot take the rating goes up as shot, and says so.
func TestRawWithoutXMPUploadsOriginal(t *testing.T) {
	h := newHarness(t)
	raw := append([]byte("II*\x00\x08\x00\x00\x00\x01\x00\x00\x01\x03\x00\x01\x00\x00\x00\x10\x00\x00\x00\x00\x00\x00\x00"),
		bytes.Repeat([]byte("data"), 100)...)
	ev := rawPhoto(t, raw, 3)
	if _, err := h.p.attempt(context.Background(), 1, ev); err != nil {
		t.Fatal(err)
	}
	if a, _ := h.lr.Get(lightroom.AssetID(ev.SHA256)); a.MasterSHA256 != ev.SHA256 || a.SHA256 != ev.SHA256 {
		t.Fatalf("asset %+v, want the original", a)
	}
	var reason string
	for _, s := range h.spans.Ended() {
		for _, kv := range s.Attributes() {
			if kv.Key == "keeper.rating.embed_error" {
				reason = kv.Value.AsString()
			}
		}
	}
	if v, ok := spanBool(h, "keeper.rating.embedded"); !ok || v || reason != "no_xmp" {
		t.Fatalf("embedded=%v (%v) reason=%q", v, ok, reason)
	}
}

// Without scratch space the raw is not uploaded unrated; it waits.
func TestRatedCopyUnwritableRetries(t *testing.T) {
	h := newHarness(t)
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	h.p.ratedDir = filepath.Join(blocker, "rated")
	raw, _ := sonyRaw(t, "blocked")
	ev := rawPhoto(t, raw, 5)
	permanent, err := h.p.attempt(context.Background(), 1, ev)
	if err == nil || permanent {
		t.Fatalf("permanent=%v err=%v, want retry", permanent, err)
	}
	if f := h.last(t).v.(events.PhotoFailed); f.ErrorClass != classSpool || f.RetryAt == nil {
		t.Fatalf("photo.failed %+v", f)
	}
	if h.lr.Calls("create_asset", http.StatusCreated) != 0 {
		t.Fatal("asset created before the master was ready")
	}
}

// A photo that gives up leaves no rated copy behind: nothing will
// redeliver it, and a refusing collection would otherwise fill the disk.
func TestRatedCopyDroppedWhenGivingUp(t *testing.T) {
	h := newAdobeHarness(t)
	h.targets.set(lrlink.Target{AlbumID: "col1", Name: "Talks"})
	h.adobe.Refuse("col1")
	raw, _ := sonyRaw(t, "refused")
	ev := rawPhoto(t, raw, 5)
	if permanent, err := h.p.attempt(context.Background(), 1, ev); err == nil || !permanent {
		t.Fatalf("permanent=%v err=%v", permanent, err)
	}
	if left, _ := filepath.Glob(filepath.Join(h.p.ratedDir, "*")); len(left) != 0 {
		t.Fatalf("rated copies left after giving up: %v", left)
	}
}

// A retry keeps the copy: the next delivery needs the same bytes.
func TestRatedCopyKeptForRetry(t *testing.T) {
	h := newHarness(t)
	raw, _ := sonyRaw(t, "retry")
	ev := rawPhoto(t, raw, 5)
	h.lr.Fail(http.StatusServiceUnavailable)
	if permanent, err := h.p.attempt(context.Background(), 1, ev); err == nil || permanent {
		t.Fatalf("permanent=%v err=%v", permanent, err)
	}
	if left, _ := filepath.Glob(filepath.Join(h.p.ratedDir, ev.SHA256+"-5.arw")); len(left) != 1 {
		t.Fatalf("rated copy gone before the retry: %v", left)
	}
}
