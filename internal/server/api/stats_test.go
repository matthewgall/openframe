package api_test

import (
	"net/http"
	"net/http/cookiejar"
	"testing"
)

// TestStatsReportLiveAndRevokedSeparately: dashboard headline numbers
// must answer "what is live right now" — trashed photos and revoked
// (terminal) devices are reported in their own fields, not mixed into
// the totals.
func TestStatsReportLiveAndRevokedSeparately(t *testing.T) {
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

	// Two live photos, one trashed.
	p1, code := admin.upload(t, makeJPEG(t), "a.jpg")
	if code != http.StatusCreated {
		t.Fatalf("upload 1: %d", code)
	}
	if _, code := admin.upload(t, makeJPEG(t), "b.jpg"); code != http.StatusCreated {
		t.Fatalf("upload 2: %d", code)
	}
	if code := admin.json(http.MethodDelete, "/api/v1/photos/"+p1, nil, nil); code != http.StatusOK {
		t.Fatalf("delete: %d", code)
	}
	// Two devices, one revoked.
	createDevice := func(name string) string {
		var out struct {
			Device struct {
				ID string `json:"id"`
			} `json:"device"`
		}
		if code := admin.json(http.MethodPost, "/api/v1/devices",
			map[string]string{"name": name}, &out); code != http.StatusCreated {
			t.Fatalf("create device %q: %d", name, code)
		}
		return out.Device.ID
	}
	_ = createDevice("Live frame")
	d2 := createDevice("Dead frame")
	if code := admin.json(http.MethodPost, "/api/v1/devices/"+d2+"/revoke", nil, nil); code != http.StatusOK {
		t.Fatalf("revoke: %d", code)
	}

	var stats struct {
		Photos struct {
			Total   int `json:"total"`
			Deleted int `json:"deleted"`
		} `json:"photos"`
		Devices struct {
			Total   int `json:"total"`
			Revoked int `json:"revoked"`
		} `json:"devices"`
	}
	if code := admin.json(http.MethodGet, "/api/v1/stats", nil, &stats); code != http.StatusOK {
		t.Fatalf("stats: %d", code)
	}
	if stats.Photos.Total != 1 || stats.Photos.Deleted != 1 {
		t.Fatalf("photos = %+v, want total 1 / deleted 1", stats.Photos)
	}
	if stats.Devices.Total != 1 || stats.Devices.Revoked != 1 {
		t.Fatalf("devices = %+v, want total 1 / revoked 1", stats.Devices)
	}
}
