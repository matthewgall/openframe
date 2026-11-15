package api_test

import (
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
)

// TestEmptyAlbumSerializesCollectionsNotNull guards the class of bug
// where an empty result serialised as JSON null (a nil slice): the
// Deleted view, the album modal and friends all crashed on
// `.length` of null. Collections in this API are "possibly empty",
// never "absent" — writeJSON normalises nil slices/maps to []/{} and
// this test pins the contract at the wire level (raw text, because
// Go unmarshalling cannot distinguish null from an empty slice).
func TestEmptyAlbumSerializesCollectionsNotNull(t *testing.T) {
	ts, dbh := newTestServer(t)
	seedUser(t, dbh, "admin@test", "admin", "pw-admin-123")
	jar, _ := cookiejar.New(nil)
	admin := &client{t: t, base: ts.URL, http: &http.Client{Jar: jar}}
	var login struct {
		CSRFToken string `json:"csrf_token"`
	}
	if code := admin.json(http.MethodPost, "/api/v1/auth/login",
		map[string]string{"email": "admin@test", "password": "pw-admin-123"}, &login); code != http.StatusOK {
		t.Fatalf("login: %d", code)
	}
	admin.csrf = login.CSRFToken

	var created struct {
		Album struct {
			ID string `json:"id"`
		} `json:"album"`
	}
	if code := admin.json(http.MethodPost, "/api/v1/albums",
		map[string]string{"name": "Empty"}, &created); code != http.StatusCreated {
		t.Fatalf("create album: %d", code)
	}

	resp, data := admin.do(http.MethodGet, "/api/v1/albums/"+created.Album.ID, nil, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get album: %d (body: %s)", resp.StatusCode, data)
	}
	s := string(data)
	if strings.Contains(s, `"photos":null`) || strings.Contains(s, `"devices":null`) {
		t.Fatalf("empty album serialised null collections: %s", s)
	}
	if !strings.Contains(s, `"photos":[]`) || !strings.Contains(s, `"devices":[]`) {
		t.Fatalf("expected [] for empty collections: %s", s)
	}
}
