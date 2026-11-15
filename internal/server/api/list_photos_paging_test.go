package api_test

import (
	"net/http"
	"net/http/cookiejar"
	"testing"
)

// TestListPhotosPaging: the library's load-more flow pages with
// limit/offset and trusts the total for the "N more" button — a
// regression here silently caps the visible library.
func TestListPhotosPaging(t *testing.T) {
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

	for _, name := range []string{"a.jpg", "b.jpg", "c.jpg"} {
		if _, code := admin.upload(t, makeJPEG(t), name); code != http.StatusCreated {
			t.Fatalf("upload %s: %d", name, code)
		}
	}

	type page struct {
		Photos []struct {
			ID string `json:"id"`
		} `json:"photos"`
		Total int `json:"total"`
	}
	get := func(offset int, out *page) {
		if code := admin.json(http.MethodGet,
			"/api/v1/photos?limit=2&offset="+itoa(offset), nil, out); code != http.StatusOK {
			t.Fatalf("list offset %d: %d", offset, code)
		}
	}

	var p1, p2 page
	get(0, &p1)
	if p1.Total != 3 || len(p1.Photos) != 2 {
		t.Fatalf("page 1: total=%d len=%d, want 3/2", p1.Total, len(p1.Photos))
	}
	get(2, &p2)
	if len(p2.Photos) != 1 {
		t.Fatalf("page 2: len=%d, want 1", len(p2.Photos))
	}
	// No overlap between pages.
	for _, a := range p1.Photos {
		for _, b := range p2.Photos {
			if a.ID == b.ID {
				t.Fatalf("pages overlap on %s", a.ID)
			}
		}
	}
	// An offset beyond the end is an empty page, not an error.
	var p3 page
	get(10, &p3)
	if p3.Total != 3 || len(p3.Photos) != 0 {
		t.Fatalf("past-end page: total=%d len=%d, want 3/0", p3.Total, len(p3.Photos))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
