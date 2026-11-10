package main

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/url"
	"strings"
	"testing"
)

// DSC07648.ARW from the author's test shoot.
const linkSHA = "93a009dcc07654d5e5b0fe81a871baebeaf94be478c54962" + "0000000000000000"

func decodeDash0State(t *testing.T, s string) map[string]any {
	t.Helper()
	raw, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zlib.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	js, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(js, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestDash0ViewLink(t *testing.T) {
	l := traceLinker{
		tmpl:    "https://app.dash0.com/traces/explorer?org=ee12526d-4f82-4dee-b3b7-e99e20d7108b&s={dash0_state}",
		dataset: "keeper-demo",
	}
	tid, link := l.link(linkSHA)
	if tid != "93a009dcc07654d5e5b0fe81a871baeb" {
		t.Fatalf("trace id %s", tid)
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("org") != "ee12526d-4f82-4dee-b3b7-e99e20d7108b" {
		t.Fatalf("org lost: %s", link)
	}
	st := decodeDash0State(t, u.Query().Get("s"))

	root := st["/"].(map[string]any)
	tr := st["/traces"].(map[string]any)
	filter := tr["query"].(map[string]any)["filter"].([]any)[0].(map[string]any)
	switch {
	case root["dataset"] != "keeper-demo":
		t.Errorf("dataset = %v", root["dataset"])
	case tr["traceId"] != "k6AJ3MB2VNXlsP6BqHG66w==": // the value in a link copied from Dash0
		t.Errorf("traceId = %v", tr["traceId"])
	case tr["id"] != base64.StdEncoding.EncodeToString([]byte{0xea, 0xf9, 0x4b, 0xe4, 0x78, 0xc5, 0x49, 0x62}):
		t.Errorf("focused span = %v, want the photo's root span", tr["id"])
	case filter["key"] != "otel.trace.id" || filter["operator"] != "is" || filter["value"] != tid:
		t.Errorf("filter = %v", filter)
	case tr["open"] != true || tr["tab"] != "overview":
		t.Errorf("waterfall not opened: %v", tr)
	case st["/traces/explorer"].(map[string]any)["elementKind"] != "span":
		t.Errorf("explorer = %v", st["/traces/explorer"])
	}
}

func TestTraceLinkPlaceholders(t *testing.T) {
	for tmpl, want := range map[string]string{
		"":                                      "",
		"https://x/{trace_id}":                  "https://x/93a009dcc07654d5e5b0fe81a871baeb",
		"https://x/{trace_id}/{span_id}":        "https://x/93a009dcc07654d5e5b0fe81a871baeb/eaf94be478c54962",
		"https://x/goto?dataset=d&t={trace_id}": "https://x/goto?dataset=d&t=93a009dcc07654d5e5b0fe81a871baeb",
	} {
		if _, got := (traceLinker{tmpl: tmpl}).link(linkSHA); got != want {
			t.Errorf("%q -> %q, want %q", tmpl, got, want)
		}
	}
	// The state is URL-escaped: base64url padding '=' must not break the query.
	_, link := traceLinker{tmpl: "https://x?s={dash0_state}", dataset: "d"}.link(linkSHA)
	if strings.Contains(strings.TrimPrefix(link, "https://x?s="), "=") {
		t.Errorf("unescaped '=' in %s", link)
	}
}

// One list of 500 photos builds 500 links; this is the per-link cost.
func BenchmarkDash0ViewLink(b *testing.B) {
	l := traceLinker{tmpl: "https://app.dash0.com/traces/explorer?org=o&s={dash0_state}", dataset: "keeper-demo"}
	b.ReportAllocs()
	for b.Loop() {
		l.link(linkSHA)
	}
}
