package api_test

import (
	"net/http"
	"net/http/cookiejar"
	"testing"
)

// TestBulkAlbumAddAndDelete: the library selection flow is batch —
// one round trip adds N photos to an album (duplicates counted as
// skipped, not errors), and one round trip trashes a selection.
func TestBulkAlbumAddAndDelete(t *testing.T) {
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

	p1, code := admin.upload(t, makeJPEG(t), "a.jpg")
	if code != http.StatusCreated {
		t.Fatalf("upload 1: %d", code)
	}
	p2, code := admin.upload(t, makeJPEG(t), "b.jpg")
	if code != http.StatusCreated {
		t.Fatalf("upload 2: %d", code)
	}
	p3, code := admin.upload(t, makeJPEG(t), "c.jpg")
	if code != http.StatusCreated {
		t.Fatalf("upload 3: %d", code)
	}
	var album struct {
		Album struct {
			ID string `json:"id"`
		} `json:"album"`
	}
	if code := admin.json(http.MethodPost, "/api/v1/albums",
		map[string]string{"name": "Trip"}, &album); code != http.StatusCreated {
		t.Fatalf("create album: %d", code)
	}

	// Batch add: two new + one duplicate id in the same request.
	var addRes struct {
		Added   int `json:"added"`
		Skipped int `json:"skipped"`
	}
	if code := admin.json(http.MethodPost, "/api/v1/albums/"+album.Album.ID+"/photos",
		map[string]any{"photo_ids": []string{p1, p2, p1}}, &addRes); code != http.StatusOK {
		t.Fatalf("bulk add: %d", code)
	}
	if addRes.Added != 2 || addRes.Skipped != 1 {
		t.Fatalf("bulk add = %+v, want added 2 / skipped 1 (duplicate)", addRes)
	}
	var detail struct {
		Photos []string `json:"photos"`
	}
	if code := admin.json(http.MethodGet, "/api/v1/albums/"+album.Album.ID, nil, &detail); code != http.StatusOK {
		t.Fatalf("album detail: %d", code)
	}
	if len(detail.Photos) != 2 {
		t.Fatalf("album photos = %v, want 2", detail.Photos)
	}

	// Missing and trashed ids are skipped, not fatal.
	if code := admin.json(http.MethodDelete, "/api/v1/photos/"+p3, nil, nil); code != http.StatusOK {
		t.Fatalf("trash p3: %d", code)
	}
	if code := admin.json(http.MethodPost, "/api/v1/albums/"+album.Album.ID+"/photos",
		map[string]any{"photo_ids": []string{p3, "no-such-photo"}}, nil); code != http.StatusOK {
		t.Fatalf("bulk add with bad ids: %d", code)
	}

	// Batch delete: one live, one already trashed.
	var delRes struct {
		Deleted int `json:"deleted"`
		Skipped int `json:"skipped"`
	}
	if code := admin.json(http.MethodPost, "/api/v1/photos/delete",
		map[string]any{"ids": []string{p1, p3}}, &delRes); code != http.StatusOK {
		t.Fatalf("bulk delete: %d", code)
	}
	if delRes.Deleted != 1 || delRes.Skipped != 1 {
		t.Fatalf("bulk delete = %+v, want deleted 1 / skipped 1 (already trashed)", delRes)
	}

	// The album keeps only what survived.
	admin.json(http.MethodGet, "/api/v1/albums/"+album.Album.ID, nil, &detail)
	if len(detail.Photos) != 1 || detail.Photos[0] != p2 {
		t.Fatalf("album after deletes = %v, want only %s", detail.Photos, p2)
	}
}
