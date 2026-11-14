package api_test // external test package: may import internal/server (no cycle)

// Live e2e for the S3 photo backend: real API + pipeline against an
// in-memory S3 stub, verifying objects land in S3 (not local disk)
// and are served back byte-identical.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/matthewgall/openframe/internal/config"
	"github.com/matthewgall/openframe/internal/db"
	"github.com/matthewgall/openframe/internal/logging"
	"github.com/matthewgall/openframe/internal/server"
	migrations "github.com/matthewgall/openframe/migrations"
)

// minifake is an in-memory S3 stub (no auth validation) for the e2e.
type minifake struct {
	mu   sync.Mutex
	objs map[string][]byte
}

func (m *minifake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	key := strings.Join(parts[1:], "/")
	switch {
	case r.URL.Query().Get("list-type") == "2":
		keys := make([]string, 0, len(m.objs))
		for k := range m.objs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteString(`<?xml version="1.0"?><ListBucketResult><IsTruncated>false</IsTruncated>`)
		for _, k := range keys {
			fmt.Fprintf(&b, `<Contents><Key>%s</Key><Size>%d</Size></Contents>`, k, len(m.objs[k]))
		}
		b.WriteString(`</ListBucketResult>`)
		io.WriteString(w, b.String())
	case r.Method == http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		m.objs[key] = body
	case r.Method == http.MethodGet:
		obj, ok := m.objs[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if rng := r.Header.Get("Range"); rng != "" {
			var off int64
			fmt.Sscanf(rng, "bytes=%d-", &off)
			if off >= int64(len(obj)) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.WriteHeader(http.StatusPartialContent)
			w.Write(obj[off:])
			return
		}
		w.Write(obj)
	case r.Method == http.MethodHead:
		obj, ok := m.objs[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(obj)))
	}
}

func TestS3E2EUploadServe(t *testing.T) {
	fake := &minifake{objs: map[string][]byte{}}
	s3ts := httptest.NewServer(fake)
	defer s3ts.Close()

	ctx := context.Background()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Server.PhotoStorage = config.PhotoStorageConfig{
		Backend: "s3",
		S3: config.S3Config{
			Endpoint:        s3ts.URL,
			Region:          "auto",
			Bucket:          "e2e",
			AccessKeyID:     "ak",
			SecretAccessKey: "sk",
			PathStyle:       true,
		},
	}
	serverDir, _, _ := cfg.Paths()
	dbh, err := db.Open(ctx, filepath.Join(serverDir, "server.db"), false)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { dbh.Close() })
	if _, err := db.MigrateFromFS(ctx, dbh, migrations.FS(), "server"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	log, err := logging.New(logging.Options{Level: "error"})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(cfg, log, dbh)
	if err != nil {
		t.Fatalf("server.New with s3: %v", err)
	}
	apiTS := httptest.NewServer(srv.Router())
	defer apiTS.Close()

	seedUser(t, dbh, "admin@example.com", "admin", "admin-pass-12345")
	admin := newClient(t, apiTS.URL, "admin@example.com", "admin-pass-12345")

	// Upload through the real API; the pipeline must store original +
	// derivatives in S3.
	jpg := makeJPEG(t)
	photoID, code := admin.upload(t, jpg, "pic.jpg")
	if code != http.StatusCreated || photoID == "" {
		t.Fatalf("upload: %d %q", code, photoID)
	}

	// Fetch the original back over HTTP (served from S3).
	resp, data := admin.do(http.MethodGet, "/api/v1/photos/"+photoID+"/file?variant=original", nil, true)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(data, jpg) {
		t.Fatalf("original via s3: %d (len %d)", resp.StatusCode, len(data))
	}

	// Objects really live in S3, not on local disk.
	fake.mu.Lock()
	keys := make([]string, 0, len(fake.objs))
	for k := range fake.objs {
		keys = append(keys, k)
	}
	fake.mu.Unlock()
	sort.Strings(keys)
	if len(keys) < 3 { // original + at least display + thumb
		t.Fatalf("expected >=3 objects in S3, got %d: %v", len(keys), keys)
	}
	if _, err := os.Stat(filepath.Join(cfg.PhotoDir(), "originals")); !os.IsNotExist(err) {
		t.Fatalf("local photos dir created despite s3 backend")
	}
}
