package manifest

import (
	"context"
	"encoding/json"
	"testing"
)

// The manifest carries photo attribution for the frame's arrival debut
// (living-presentation R12/R7): uploader display name and the
// server-side receipt time, additive within openframe-manifest/1.
func TestBuildCarriesUploaderFields(t *testing.T) {
	ctx := context.Background()
	dbh, store, state := seedBenchmark(t, 2)

	m, _, err := Build(ctx, dbh, store, "dev1", state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Albums) != 1 || len(m.Albums[0].Photos) != 2 {
		t.Fatalf("unexpected manifest shape: %v", m.Albums)
	}
	p := m.Albums[0].Photos[0]
	// seedBenchmark inserts user u1 with display_name "U".
	if p.Uploader != "U" {
		t.Errorf("uploader = %q, want U", p.Uploader)
	}
	if p.UploaderAt == "" {
		t.Error("uploaded_at empty; expected seeded created_at passthrough")
	}
	if p.Hash == "" || p.Display == nil {
		t.Fatal("baseline fields lost while adding new ones")
	}

	// JSON surface: fields present and named per the protocol comment.
	b, _ := json.Marshal(p)
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["uploader"] != "U" || raw["uploaded_at"] == "" || raw["uploaded_at"] == nil {
		t.Errorf("json = %s", b)
	}

	// Empty display name (users.display_name is NOT NULL but can be
	// blank) degrades to an empty uploader field; the photo still
	// ships. (A truly orphaned uploaded_by is FK-blocked via the app;
	// the COALESCE covers out-of-band DB surgery.)
	if _, err := dbh.ExecContext(ctx, `UPDATE users SET display_name = '' WHERE id = 'u1'`); err != nil {
		t.Fatal(err)
	}
	m2, _, err := Build(ctx, dbh, store, "dev1", state, nil)
	if err != nil {
		t.Fatal(err)
	}
	p2 := m2.Albums[0].Photos[0]
	if p2.Uploader != "" {
		t.Errorf("uploader = %q for blank display name, want empty", p2.Uploader)
	}
	if p2.Hash != p.Hash {
		t.Error("photo lost when its uploader name blanked")
	}
	// File resources unchanged (the fields bump content/revision only,
	// never the downloadable bytes).
	if p2.Display.Resource != p.Display.Resource || p2.Display.SHA256 != p.Display.SHA256 {
		t.Error("display file descriptor changed for identical content")
	}
}
