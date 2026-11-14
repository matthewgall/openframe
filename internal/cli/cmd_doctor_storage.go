package cli

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/matthewgall/openframe/internal/config"
	"github.com/matthewgall/openframe/internal/server/storage"
)

// checkPhotoStorage verifies the configured photo object backend
// (doctor is read-only: the local check stats, never creates).
func checkPhotoStorage(cfg *config.Config, add func(string, checkStatus, string)) {
	switch cfg.Server.PhotoStorage.Backend {
	case "", "local":
		dir := cfg.PhotoDir()
		if st, err := os.Stat(dir); err == nil {
			_ = st
			add("photo storage", checkOK, "local: "+dir)
		} else {
			add("photo storage", checkWarn, "local: "+dir+" (not created yet; the server creates it on first run)")
		}
	case "s3":
		s := cfg.Server.PhotoStorage.S3
		backend, err := storage.NewS3(s, &http.Client{Timeout: 15 * time.Second})
		if err != nil {
			add("photo storage", checkFail, "s3: "+err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := backend.Ping(ctx); err != nil {
			add("photo storage", checkFail, fmt.Sprintf("s3: bucket %q unreachable: %s", s.Bucket, shortErr(err)))
			return
		}
		add("photo storage", checkOK, fmt.Sprintf("s3: bucket %q reachable", s.Bucket))
	default:
		add("photo storage", checkFail, "unknown backend "+cfg.Server.PhotoStorage.Backend)
	}
}
