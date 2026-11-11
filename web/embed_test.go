package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func doGet(t *testing.T, h http.Handler, path string) (int, string, string) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	body, _ := io.ReadAll(res.Result().Body)
	return res.Code, res.Result().Header.Get("Content-Type"), string(body)
}

func TestSPAServesIndexAtRoot(t *testing.T) {
	code, ct, body := doGet(t, SPAHandler(), "/")
	if code != 200 || !strings.Contains(ct, "text/html") || !strings.Contains(body, "<!doctype html>") {
		t.Errorf("root: code=%d ct=%q body=%.40q", code, ct, body)
	}
}

func TestSPAFallbackForExtensionless(t *testing.T) {
	code, _, body := doGet(t, SPAHandler(), "/library")
	if code != 200 || !strings.Contains(body, "<!doctype html>") {
		t.Errorf("spa fallback: code=%d", code)
	}
}

func TestSPA404sMissingFiles(t *testing.T) {
	code, _, _ := doGet(t, SPAHandler(), "/nope.js")
	if code != 404 {
		t.Errorf("missing asset: code=%d, want 404", code)
	}
}

func TestSPA404sAPIPaths(t *testing.T) {
	code, ct, body := doGet(t, SPAHandler(), "/api/v1/photos")
	if code != 404 || !strings.Contains(ct, "application/json") || !strings.Contains(body, `"error"`) {
		t.Errorf("api 404: code=%d ct=%q body=%q", code, ct, body)
	}
}

func TestNotFoundHandlerRoutes(t *testing.T) {
	code, ct, _ := doGet(t, NotFoundHandler(), "/api/nope")
	if code != 404 || !strings.Contains(ct, "application/json") {
		t.Errorf("api: code=%d ct=%q", code, ct)
	}
	code, _, _ = doGet(t, NotFoundHandler(), "/unknown-page")
	if code != 200 {
		t.Errorf("extensionless fallback: code=%d, want 200", code)
	}
	code, _, _ = doGet(t, NotFoundHandler(), "/missing.css")
	if code != 404 {
		t.Errorf("missing css: code=%d, want 404", code)
	}
}

func TestBuiltAssetsPresent(t *testing.T) {
	if len(distFiles) == 0 {
		t.Fatal("no built assets embedded — run `make web`")
	}
	if _, ok := distFiles["index.html"]; !ok {
		t.Error("index.html missing from embedded dist")
	}
	foundAsset := false
	for name := range distFiles {
		if strings.HasPrefix(name, "assets/") {
			foundAsset = true
			break
		}
	}
	if !foundAsset {
		t.Error("no assets/ files in embedded dist")
	}
}
