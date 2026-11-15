package api_test

import (
	"net/http"
	"net/http/cookiejar"
	"testing"
)

// TestRevokeDeviceUnassignsFromAlbums: revocation is terminal (a
// revoked device can never be issued credentials again), so the
// endpoint must strip the device from every album — otherwise dead
// frames keep claiming collections in the assignment UI (visibly
// confusing when a live frame shares the dead one's name).
func TestRevokeDeviceUnassignsFromAlbums(t *testing.T) {
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

	var album struct {
		Album struct {
			ID string `json:"id"`
		} `json:"album"`
	}
	if code := admin.json(http.MethodPost, "/api/v1/albums",
		map[string]string{"name": "Holiday"}, &album); code != http.StatusCreated {
		t.Fatalf("create album: %d", code)
	}
	var dev struct {
		Device struct {
			ID string `json:"id"`
		} `json:"device"`
	}
	if code := admin.json(http.MethodPost, "/api/v1/devices",
		map[string]string{"name": "Living room (test)"}, &dev); code != http.StatusCreated {
		t.Fatalf("create device: %d", code)
	}

	if code := admin.json(http.MethodPost,
		"/api/v1/albums/"+album.Album.ID+"/frames/"+dev.Device.ID, nil, nil); code != http.StatusOK {
		t.Fatalf("assign: %d", code)
	}
	var detail struct {
		Devices []string `json:"devices"`
	}
	if code := admin.json(http.MethodGet, "/api/v1/albums/"+album.Album.ID, nil, &detail); code != http.StatusOK {
		t.Fatalf("album detail: %d", code)
	}
	if len(detail.Devices) != 1 || detail.Devices[0] != dev.Device.ID {
		t.Fatalf("devices = %v, want the new device", detail.Devices)
	}

	var rev struct {
		Status     string `json:"status"`
		Unassigned int64  `json:"unassigned_from_albums"`
	}
	if code := admin.json(http.MethodPost, "/api/v1/devices/"+dev.Device.ID+"/revoke",
		nil, &rev); code != http.StatusOK {
		t.Fatalf("revoke: %d", code)
	}
	if rev.Status != "revoked" || rev.Unassigned != 1 {
		t.Fatalf("revoke response = %+v, want revoked/1", rev)
	}

	admin.json(http.MethodGet, "/api/v1/albums/"+album.Album.ID, nil, &detail)
	if len(detail.Devices) != 0 {
		t.Fatalf("revoked device still assigned: %v", detail.Devices)
	}
}
