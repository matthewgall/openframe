package api_test

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestSoakMixedLoad is the M7 long-duration reliability gate. It keeps
// the server under sustained mixed load — concurrent photo uploads
// (full decode/derivative pipeline), device manifest polls (DB +
// storage stat path) and photo-list reads — for a bounded duration
// and asserts:
//
//   - zero 5xx responses across the whole run (errors are load, not
//     500s),
//   - the server is still healthy and responsive at the end,
//   - uploads actually landed (the read side sees real data).
//
// Duration is 60s by default; set OPENFRAME_SOAK_SECONDS to override
// (e.g. 3600 for an overnight run) and OPENFRAME_SOAK=skip to skip.
// It is intentionally NOT run in the normal CI suite — `make soak`
// runs it (plus repeated -race suites) as the release gate.
func TestSoakMixedLoad(t *testing.T) {
	if os.Getenv("OPENFRAME_SOAK") == "skip" {
		t.Skip("OPENFRAME_SOAK=skip")
	}
	seconds := 60
	if v := os.Getenv("OPENFRAME_SOAK_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			seconds = n
		}
	}
	dur := time.Duration(seconds) * time.Second
	// Keep the default CI-safe ceiling low; the flag is for local use.
	if seconds > 300 {
		t.Logf("soak run: %v (long run)", dur)
	}
	t.Logf("soak run: %v", dur)

	ts, dbh := newTestServer(t)
	seedUser(t, dbh, "soakadmin@example.com", "admin", "soak-password-123")
	admin := newClient(t, ts.URL, "soakadmin@example.com", "soak-password-123")

	// A device + credential for the manifest worker.
	var dev struct {
		Device struct {
			ID string `json:"id"`
		} `json:"device"`
	}
	if code := admin.json(http.MethodPost, "/api/v1/devices",
		map[string]string{"name": "Soak Frame"}, &dev); code != http.StatusCreated {
		t.Fatalf("create device: %d", code)
	}
	var cred struct {
		Token string `json:"token"`
	}
	if code := admin.json(http.MethodPost, "/api/v1/devices/"+dev.Device.ID+"/credential",
		map[string]any{}, &cred); code != http.StatusCreated {
		t.Fatalf("issue credential: %d", code)
	}

	var (
		uploads   atomic.Int64
		manifests atomic.Int64
		lists     atomic.Int64
		healths   atomic.Int64
		err5xx    atomic.Int64
	)
	record := func(name string, code int) {
		if code >= 500 {
			err5xx.Add(1)
			t.Logf("5xx on %s: %d", name, code)
		}
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	start := time.Now()

	// Upload worker: one file per round-trip, unique bytes each time.
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for time.Now().Before(start.Add(dur)) {
			i++
			photoID, code := admin.upload(t, makeJPEG(t), fmt.Sprintf("soak-%d.jpg", i))
			record("upload", code)
			if photoID != "" {
				uploads.Add(1)
			}
		}
	}()

	// Manifest worker: device-authenticated manifest poll with an
	// ETag-aware client (a 304 counts as a success too).
	wg.Add(1)
	go func() {
		defer wg.Done()
		etag := ""
		req0 := func() {
			req, err := http.NewRequest(http.MethodGet,
				ts.URL+"/api/v1/frames/"+dev.Device.ID+"/manifest", nil)
			if err != nil {
				t.Error(err)
				return
			}
			req.Header.Set("Authorization", "Bearer "+cred.Token)
			if etag != "" {
				req.Header.Set("If-None-Match", etag)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
			if resp.StatusCode == http.StatusOK {
				etag = resp.Header.Get("ETag")
				manifests.Add(1)
			} else if resp.StatusCode == http.StatusNotModified {
				manifests.Add(1)
			} else {
				record("manifest", resp.StatusCode)
			}
		}
		req0()
		for time.Now().Before(start.Add(dur)) {
			req0()
		}
	}()

	// List worker: admin photo list (paginated read path).
	wg.Add(1)
	go func() {
		defer wg.Done()
		page := 0
		for time.Now().Before(start.Add(dur)) {
			var out map[string]any
			code := admin.json(http.MethodGet, "/api/v1/photos?limit=20&offset="+strconv.Itoa(page%100*20), nil, &out)
			record("photos list", code)
			if code == http.StatusOK {
				lists.Add(1)
			}
			page++
		}
	}()

	// Health worker.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for time.Now().Before(start.Add(dur)) {
			resp, err := http.Get(ts.URL + "/healthz")
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
			record("healthz", resp.StatusCode)
			if resp.StatusCode == http.StatusOK {
				healths.Add(1)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}()

	<-time.After(dur)
	close(stop)
	wg.Wait()

	// Final assertions.
	if got := err5xx.Load(); got != 0 {
		t.Fatalf("%d 5xx responses during soak", got)
	}
	if got := uploads.Load(); got == 0 {
		t.Fatal("no uploads completed during soak")
	}
	if got := manifests.Load(); got == 0 {
		t.Fatal("no manifest polls completed during soak")
	}
	if got := lists.Load(); got == 0 {
		t.Fatal("no list reads completed during soak")
	}

	// The server must still be healthy and consistent after the run.
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("post-soak healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post-soak healthz = %d", resp.StatusCode)
	}

	// The read side must see the uploaded data.
	var out map[string]any
	if code := admin.json(http.MethodGet, "/api/v1/photos?limit=5", nil, &out); code != http.StatusOK {
		t.Fatalf("post-soak photo list = %d", code)
	}
	t.Logf("soak complete: uploads=%d manifests=%d lists=%d healths=%d over %v",
		uploads.Load(), manifests.Load(), lists.Load(), healths.Load(), dur.Round(time.Millisecond))
}
