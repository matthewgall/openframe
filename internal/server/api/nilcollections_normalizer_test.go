package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNormalizeNilCollections(t *testing.T) {
	// The contract, including the sharp edges.
	now := time.Now().UTC().Truncate(time.Second)
	raw := json.RawMessage(`{"version":"1.0.0","signature":"abc"}`)

	v := map[string]any{
		"list":     []string(nil),
		"nested":   map[string]any{"inner": []int(nil)},
		"elements": []any{1, map[string]any{"s": []float64(nil)}},
		"when":     now,
		"raw":      raw,
		"b64":      []byte{0x01, 0x02},
		"nothing":  (*string)(nil),
		"opaque":   struct{ Data []byte }{Data: []byte{9}},
	}
	got := normalizeNilCollections(v).(map[string]any)

	enc, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(enc)

	for _, want := range []string{
		`"list":[]`,
		`"inner":[]`,
		`"s":[]`,
		`"when":"` + now.Format(time.RFC3339) + `"`,
		`"raw":{"version":"1.0.0","signature":"abc"}`,
		`"b64":"AQI="`, // base64, not [1,2]
		`"nothing":null`,
		`"opaque":{"Data":"CQ=="}`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %s — got: %s", want, s)
		}
	}
	for _, banned := range []string{`"list":null`, `"inner":null`, `[1,2]`} {
		if strings.Contains(s, banned) {
			t.Errorf("output must not contain %s — got: %s", banned, s)
		}
	}
}

func TestNormalizeNilCollectionsTopLevelStruct(t *testing.T) {
	type resp struct {
		ID    string
		Items []string
		When  time.Time
		Raw   json.RawMessage
	}
	in := resp{ID: "x", Items: nil, When: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Raw: json.RawMessage(`[1,2]`)}
	got := normalizeNilCollections(in).(resp)
	if got.Items == nil {
		t.Fatalf("Items is nil, want empty non-nil slice")
	}
	if len(got.Items) != 0 {
		t.Fatalf("Items = %#v, want empty", got.Items)
	}
	if !got.When.Equal(in.When) {
		t.Fatalf("When changed: %v", got.When)
	}
	if string(got.Raw) != `[1,2]` {
		t.Fatalf("Raw mangled: %s", got.Raw)
	}
}

// TestNormalizeNilCollectionsPointerFields covers the manifest.Config
// shape: *int / *bool / *struct fields, nil and set. The normaliser
// must preserve the pointer type (assigning the bare pointee into a
// pointer field panics) and keep nil pointers null.
func TestNormalizeNilCollectionsPointerFields(t *testing.T) {
	bri := 80
	on := false
	type file struct {
		Resource string   `json:"resource"`
		Tags     []string `json:"tags"`
	}
	type cfg struct {
		Brightness *int    `json:"brightness,omitempty"`
		ShowClock  *bool   `json:"show_clock,omitempty"`
		Cover      *file   `json:"cover,omitempty"`
		Nickname   *string `json:"nickname,omitempty"`
	}
	in := cfg{Brightness: &bri, ShowClock: &on, Cover: &file{Resource: "r", Tags: nil}, Nickname: nil}
	got := normalizeNilCollections(in).(cfg)
	if got.Brightness == nil || *got.Brightness != 80 {
		t.Fatalf("Brightness = %#v, want *80", got.Brightness)
	}
	if got.ShowClock == nil || *got.ShowClock != false {
		t.Fatalf("ShowClock = %#v, want *false", got.ShowClock)
	}
	if got.Cover == nil || got.Cover.Resource != "r" || got.Cover.Tags == nil {
		t.Fatalf("Cover = %#v, want non-nil with normalised tags", got.Cover)
	}
	if got.Nickname != nil {
		t.Fatalf("Nickname = %#v, want nil (null)", got.Nickname)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `"brightness":80`) || !strings.Contains(s, `"tags":[]`) {
		t.Errorf("unexpected marshal: %s", s)
	}
	if strings.Contains(s, `"nickname"`) {
		t.Errorf("nil pointer must omit under omitempty: %s", s)
	}
}

// TestNormalizeNilCollectionsTypedSliceInStruct covers the manifest
// shape (Manifest.Albums []Album, Album.Photos []Photo): a typed
// slice nested in a struct must come back as the SAME type, with
// inner nil slices normalised — returning []any used to panic the
// parent field Set.
func TestNormalizeNilCollectionsTypedSliceInStruct(t *testing.T) {
	type photo struct {
		ID   string   `json:"id"`
		Tags []string `json:"tags"`
	}
	type album struct {
		ID     string  `json:"id"`
		Photos []photo `json:"photos"`
	}
	type manifest struct {
		Protocol string  `json:"protocol"`
		Albums   []album `json:"albums"`
	}
	in := manifest{Protocol: "of-sync-1", Albums: []album{{ID: "a", Photos: nil}, {ID: "b", Photos: []photo{{ID: "p1", Tags: nil}}}}}
	got := normalizeNilCollections(in).(manifest)
	if got.Albums[0].Photos == nil {
		t.Fatalf("Albums[0].Photos is nil, want empty non-nil")
	}
	if got.Albums[1].Photos[0].Tags == nil {
		t.Fatalf("Tags is nil, want empty non-nil")
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"photos":[]`) || !strings.Contains(string(b), `"tags":[]`) {
		t.Errorf("unexpected marshal: %s", b)
	}
}
