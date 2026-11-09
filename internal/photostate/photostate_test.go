package photostate

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/geilername/keeper/internal/events"
)

func TestAdvanceNeverMovesBackwards(t *testing.T) {
	all := []Stage{StageNone, StageIngested, StageScored, StageDecided, StagePublishing,
		StageRetrying, StageFailed, StagePublished}
	for _, cur := range all {
		for _, next := range all {
			got := advance(cur, next)
			if rank[got] < rank[cur] {
				t.Errorf("advance(%q, %q) = %q: moved backwards", cur, next, got)
			}
			want := cur
			if rank[next] > rank[cur] {
				want = next
			}
			if got != want {
				t.Errorf("advance(%q, %q) = %q, want %q", cur, next, got, want)
			}
		}
	}
}

func TestEveryStageHasARank(t *testing.T) {
	seen := map[int]Stage{}
	for s, r := range rank {
		if other, dup := seen[r]; dup {
			t.Errorf("stages %q and %q share rank %d", s, other, r)
		}
		seen[r] = s
	}
}

const testSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

var (
	t0       = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	retryAt  = t0.Add(time.Minute)
	ingested = &events.PhotoIngested{SHA256: testSHA, FileName: "DSC0001.jpg", CapturedAt: t0}
	scored   = &events.PhotoScored{SHA256: testSHA, CapturedAt: t0,
		Features: events.Features{DHash: "00ff00ff00ff00ff"}}
	decided = &events.PhotoDecided{SHA256: testSHA, BurstID: "b1", Bucket: events.BucketBest,
		Rating: 5, Reasons: []string{"sharpest"}}
	retry1    = &events.PhotoFailed{SHA256: testSHA, Attempt: 1, ErrorClass: "lightroom_5xx", RetryAt: &retryAt}
	retry2    = &events.PhotoFailed{SHA256: testSHA, Attempt: 2, ErrorClass: "timeout", RetryAt: &retryAt}
	gaveUp    = &events.PhotoFailed{SHA256: testSHA, Attempt: 8, ErrorClass: "lightroom_5xx"}
	published = &events.PhotoPublished{SHA256: testSHA, LightroomAssetID: testSHA[:32], Attempt: 3}
)

// fold applies events in order, one second apart, like a stream would.
func fold(evs ...any) (Photo, []bool) {
	p := Photo{SHA256: testSHA}
	var first []bool
	for i, ev := range evs {
		var fp bool
		p, fp = Apply(p, ev, t0.Add(time.Duration(i+1)*time.Second))
		first = append(first, fp)
	}
	return p, first
}

func TestApplyStages(t *testing.T) {
	tests := []struct {
		name string
		evs  []any
		want Stage
	}{
		{"ingested", []any{ingested}, StageIngested},
		{"happy path", []any{ingested, scored, decided, published}, StagePublished},
		{"retrying", []any{ingested, scored, decided, retry1}, StageRetrying},
		{"retry then success", []any{decided, retry1, retry2, published}, StagePublished},
		{"gave up", []any{decided, retry1, gaveUp}, StageFailed},
		{"failed after published stays published", []any{decided, published, retry2}, StagePublished},
		{"final failure after published stays published", []any{decided, published, gaveUp}, StagePublished},
		{"published after final failure wins", []any{decided, gaveUp, published}, StagePublished},
		{"retry after final failure stays failed", []any{decided, gaveUp, retry1}, StageFailed},
		{"late ingested after decided", []any{scored, decided, ingested}, StageDecided},
		{"late scored after published", []any{ingested, decided, published, scored}, StagePublished},
		{"redelivered decided after retrying", []any{decided, retry1, decided}, StageRetrying},
		{"scored first", []any{scored}, StageScored},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, _ := fold(tt.evs...)
			if p.Stage != tt.want {
				t.Errorf("stage = %q, want %q", p.Stage, tt.want)
			}
		})
	}
}

func TestLateEventsStillFillTheirFields(t *testing.T) {
	p, _ := fold(published, decided, scored, ingested)
	if p.Stage != StagePublished {
		t.Fatalf("stage = %q", p.Stage)
	}
	if p.FileName != "DSC0001.jpg" || p.Rating != 5 || p.Features == nil ||
		p.LightroomAssetID == "" || p.CapturedAt == nil {
		t.Errorf("late events lost data: %+v", p)
	}
	if !p.SeenAt.Equal(t0.Add(time.Second)) {
		t.Errorf("seen_at = %v, want the first event's time", p.SeenAt)
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	for _, ev := range []any{ingested, scored, decided, retry1, gaveUp, published} {
		once, _ := fold(ingested, ev)
		twice, _ := Apply(once, ev, t0.Add(time.Hour)) // redelivered much later
		if !reflect.DeepEqual(once, twice) {
			t.Errorf("%T applied twice changed the photo:\n once  %+v\n twice %+v", ev, once, twice)
		}
	}
}

func TestOlderFailureDoesNotOverwriteNewer(t *testing.T) {
	p, _ := fold(decided, retry2, retry1)
	if p.Attempts != 2 || p.LastError != "timeout" {
		t.Errorf("attempts=%d last_error=%q, want 2 timeout", p.Attempts, p.LastError)
	}
}

func TestFirstPublishedOnlyOnce(t *testing.T) {
	_, first := fold(ingested, published, published, gaveUp, published)
	want := []bool{false, true, false, false, false}
	if !reflect.DeepEqual(first, want) {
		t.Errorf("firstPublished = %v, want %v", first, want)
	}
}

func TestDecodePhotoEvent(t *testing.T) {
	sha, ev, err := Decode(events.SubjectPublished,
		[]byte(`{"v":1,"sha256":"`+testSHA+`","lightroom_asset_id":"x","attempt":1}`))
	if err != nil || sha != testSHA {
		t.Fatalf("sha=%q err=%v", sha, err)
	}
	if _, ok := ev.(*events.PhotoPublished); !ok {
		t.Fatalf("ev = %T", ev)
	}
	if _, _, err := Decode(events.SubjectScored, []byte(`{"sha256":"../etc"}`)); err == nil {
		t.Error("invalid sha accepted")
	}
	if _, _, err := Decode(events.SubjectScored, []byte(`not json`)); err == nil {
		t.Error("invalid json accepted")
	}
	if _, ev, err := Decode("photo.unknown", []byte(`{}`)); ev != nil || err != nil {
		t.Errorf("unknown subject: ev=%v err=%v", ev, err)
	}
}

func TestValidSHA(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{testSHA, true},
		{strings.Repeat("0", 64), true},
		{strings.ToUpper(testSHA), false},
		{testSHA[:63], false},
		{testSHA + "0", false},
		{testSHA[:62] + "zz", false},
		{"", false},
		{strings.Repeat(".", 64), false},
	}
	for _, tt := range tests {
		if got := ValidSHA(tt.in); got != tt.want {
			t.Errorf("ValidSHA(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
