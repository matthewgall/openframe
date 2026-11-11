package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openframe/openframe/internal/config"
	"github.com/openframe/openframe/internal/db"
	migrations "github.com/openframe/openframe/migrations"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()
	dbh, err := db.Open(ctx, t.TempDir()+"/server.db", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbh.Close() })
	if _, err := db.MigrateFromFS(ctx, dbh, migrations.FS(), "server"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), dbh)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("not JSON (%d): %s", res.StatusCode, data)
	}
	return res.StatusCode, m
}

func TestHealthzOK(t *testing.T) {
	s := newTestServer(t)
	ts := httptest.NewServer(s.Router())
	defer ts.Close()

	code, m := getJSON(t, ts.URL+"/healthz")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if m["status"] != "ok" {
		t.Errorf("status field = %v", m["status"])
	}
	if m["version"] == "" {
		t.Error("version missing")
	}
	dbh, _ := m["db"].(map[string]any)
	if dbh["status"] != "ok" {
		t.Errorf("db status = %v", dbh)
	}
}

func TestHealthzDegradedWhenDBDown(t *testing.T) {
	s := newTestServer(t)
	ts := httptest.NewServer(s.Router())
	defer ts.Close()

	// Kill the database out from under the server.
	s.db.Close()

	code, m := getJSON(t, ts.URL+"/healthz")
	if code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", code)
	}
	if m["status"] != "degraded" {
		t.Errorf("status field = %v, want degraded", m["status"])
	}
}

func TestSPAServing(t *testing.T) {
	s := newTestServer(t)
	ts := httptest.NewServer(s.Router())
	defer ts.Close()

	cases := []struct {
		path   string
		want   int
		prefix string
	}{
		{"/", http.StatusOK, "<!doctype html>"},
		{"/library", http.StatusOK, "<!doctype html>"}, // SPA fallback
		{"/assets/index.js", http.StatusNotFound, ""},  // missing asset
		{"/api/nope", http.StatusNotFound, `"error"`},  // API 404 JSON
	}
	for _, tc := range cases {
		res, err := http.Get(ts.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != tc.want {
			t.Errorf("%s: status = %d, want %d", tc.path, res.StatusCode, tc.want)
		}
		if tc.prefix != "" && !strings.Contains(string(body), tc.prefix) {
			t.Errorf("%s: body missing %q: %.80s", tc.path, tc.prefix, body)
		}
	}

	// A real built asset must be served with the right content type.
	res, err := http.Get(ts.URL + "/assets/" + firstAssetJS(t))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("built asset: %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("asset content type = %q", ct)
	}
}

func firstAssetJS(t *testing.T) string {
	t.Helper()
	// Read the committed dist listing via the embedded FS through the
	// package under test's handler is not exposed; instead locate any
	// asset by probing the known build output pattern. The Vite build
	// hashes file names, so we fetch "/" and parse the script src.
	s := newTestServer(t)
	ts := httptest.NewServer(s.Router())
	defer ts.Close()
	res, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	const marker = `src="./assets/`
	i := strings.Index(string(body), marker)
	if i < 0 {
		t.Fatal("index.html has no script src")
	}
	rest := string(body)[i+len(marker):]
	end := strings.IndexAny(rest, `"')`)
	name := rest[:end]
	return name
}

func TestRunGracefulShutdown(t *testing.T) {
	s := newTestServer(t)

	// Bind an ephemeral port to avoid collisions in parallel tests.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	s.cfg.Server.Listen = "127.0.0.1:" + strconv.Itoa(port)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Wait for readiness.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		res, err := http.Get("http://" + s.cfg.Server.Listen + "/healthz")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned error on clean shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}

	// Listener must be gone.
	if _, err := net.DialTimeout("tcp", s.cfg.Server.Listen, 200*time.Millisecond); err == nil {
		t.Error("port still open after shutdown")
	}
}
