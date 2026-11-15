package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fakeArtifact = "fake-binary-bytes-for-release-tests"

func genTestKey(t *testing.T, dir, keyID string) (string, string) {
	t.Helper()
	if code := releaseKeygen([]string{"--key-id", keyID, "--out-dir", dir}); code != ExitOK {
		t.Fatalf("keygen: exit %d", code)
	}
	privPath := filepath.Join(dir, "release-key-"+keyID+".private")
	trustPath := filepath.Join(dir, "trusted-keys-"+keyID+".json")
	for _, p := range []string{privPath, trustPath} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("keygen output missing %s: %v", p, err)
		}
		if perm := st.Mode().Perm(); perm != 0o600 {
			t.Fatalf("%s mode = %o, want 0600", p, perm)
		}
	}
	return privPath, trustPath
}

func signTestRelease(t *testing.T, dir, privPath, outName string) string {
	t.Helper()
	artifact := filepath.Join(dir, "bin")
	if err := os.WriteFile(artifact, []byte(fakeArtifact), 0o755); err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(dir, outName)
	code := releaseSign([]string{
		"--version", "0.2.0", "--channel", "stable",
		"--os", "linux", "--arch", "amd64",
		"--artifact", artifact, "--key", privPath,
		"--out", metaPath,
	})
	if code != ExitOK {
		t.Fatalf("sign: exit %d", code)
	}
	return metaPath
}

func TestReleaseKeygenSignVerify(t *testing.T) {
	dir := t.TempDir()
	privPath, trustPath := genTestKey(t, dir, "test-key")
	metaPath := signTestRelease(t, dir, privPath, "meta.json")

	// Valid pair verifies.
	if code := releaseVerify([]string{"--metadata", metaPath, "--trust", trustPath}); code != ExitOK {
		t.Fatalf("verify: exit %d, want 0", code)
	}

	// Tampered metadata fails.
	b, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(b), `"version": "0.2.0"`, `"version": "9.9.9"`, 1)
	if tampered == string(b) {
		t.Fatal("test setup: version not found in metadata")
	}
	tamperedPath := filepath.Join(dir, "tampered.json")
	if err := os.WriteFile(tamperedPath, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := releaseVerify([]string{"--metadata", tamperedPath, "--trust", trustPath}); code != ExitError {
		t.Fatalf("verify tampered: exit %d, want %d", code, ExitError)
	}

	// A release signed by a DIFFERENT key must fail against this trust
	// file — the device-side rejection path.
	otherPriv, _ := genTestKey(t, dir, "other-key")
	otherMeta := signTestRelease(t, dir, otherPriv, "other.json")
	if code := releaseVerify([]string{"--metadata", otherMeta, "--trust", trustPath}); code != ExitError {
		t.Fatalf("verify foreign signer: exit %d, want %d", code, ExitError)
	}

	// Sign with a bad key file fails cleanly.
	if code := releaseSign([]string{"--version", "0.2.0", "--channel", "stable", "--os", "linux",
		"--arch", "amd64", "--artifact", filepath.Join(dir, "bin"), "--key", "/nonexistent"}); code != ExitError {
		t.Fatalf("sign bad key: exit %d, want %d", code, ExitError)
	}
}

func TestReleasePublish(t *testing.T) {
	dir := t.TempDir()
	privPath, _ := genTestKey(t, dir, "test-key")
	metaPath := signTestRelease(t, dir, privPath, "meta.json")
	metaBytes, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}

	var gotMeta, gotArtifact []byte
	var gotCSRF, gotCookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/login":
			w.Header().Set("Set-Cookie", "of_session=test; Path=/")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"csrf_token":"csrf-123"}`)
		case "/api/v1/releases":
			gotCSRF = r.Header.Get("X-CSRF-Token")
			gotCookie = r.Header.Get("Cookie")
			if err := r.ParseMultipartForm(64 << 20); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			metaField := r.FormValue("metadata")
			gotMeta = []byte(metaField)
			file, header, err := r.FormFile("artifact")
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			defer file.Close()
			buf := new(bytes.Buffer)
			if _, err := buf.ReadFrom(file); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			gotArtifact = buf.Bytes()
			if header.Size != int64(len(fakeArtifact)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusCreated)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"version":"0.2.0","status":"pending"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	code := releasePublish([]string{
		"--server", srv.URL, "--email", "admin@test", "--password", "pw-admin-123",
		"--metadata", metaPath, "--artifact", filepath.Join(dir, "bin"),
	})
	if code != ExitOK {
		t.Fatalf("publish: exit %d", code)
	}
	if gotCSRF != "csrf-123" {
		t.Fatalf("csrf header = %q, want csrf-123", gotCSRF)
	}
	if !strings.Contains(gotCookie, "of_session=test") {
		t.Fatalf("session cookie not sent: %q", gotCookie)
	}
	if !bytes.Equal(gotMeta, metaBytes) {
		t.Fatal("server received different metadata bytes than signed (verbatim store broken)")
	}
	if !bytes.Equal(gotArtifact, []byte(fakeArtifact)) {
		t.Fatal("server received a different artifact")
	}
}

func TestReleasePublishLoginFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error_code": "bad_credentials"})
	}))
	defer srv.Close()

	dir := t.TempDir()
	privPath, _ := genTestKey(t, dir, "test-key")
	metaPath := signTestRelease(t, dir, privPath, "meta.json")
	code := releasePublish([]string{
		"--server", srv.URL, "--email", "admin@test", "--password", "wrong",
		"--metadata", metaPath, "--artifact", filepath.Join(dir, "bin"),
	})
	if code != ExitError {
		t.Fatalf("publish with bad login: exit %d, want %d", code, ExitError)
	}
}
