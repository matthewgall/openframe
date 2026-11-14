// Package server implements the central management server.
//
// M1 provides: configuration bootstrap, database migrations, the
// /healthz endpoint and static serving of the embedded admin SPA.
//
// M2+ adds the REST API under /api/v1 (auth, users, photos, albums,
// devices, releases) in sub-packages: internal/server/{auth,users,
// photos,albums,devices,releases,storage,api}. Those packages must not
// import display/graphics code — the server binary never links frame
// hardware dependencies (spec §3).
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/matthewgall/openframe/internal/config"
	"github.com/matthewgall/openframe/internal/server/albums"
	"github.com/matthewgall/openframe/internal/server/api"
	"github.com/matthewgall/openframe/internal/server/audit"
	"github.com/matthewgall/openframe/internal/server/auth"
	"github.com/matthewgall/openframe/internal/server/devices"
	"github.com/matthewgall/openframe/internal/server/pairing"
	"github.com/matthewgall/openframe/internal/server/photos"
	"github.com/matthewgall/openframe/internal/server/releases"
	"github.com/matthewgall/openframe/internal/server/storage"
	"github.com/matthewgall/openframe/internal/server/users"
	"github.com/matthewgall/openframe/internal/version"
	"github.com/matthewgall/openframe/web"
)

// Server is the management server instance.
type Server struct {
	cfg       *config.Config
	log       *slog.Logger
	db        *sql.DB
	api       *api.Server
	userStore *users.Store
	httpS     *http.Server
	start     time.Time
}

// New builds a Server. The db must already be migrated.
func New(cfg *config.Config, log *slog.Logger, dbh *sql.DB) (*Server, error) {
	if cfg == nil {
		return nil, fmt.Errorf("server: nil config")
	}
	if dbh == nil {
		return nil, fmt.Errorf("server: nil db")
	}

	// Photo object storage: local filesystem (default) or S3-compatible
	// (server.photo_storage.backend, ADR-0012).
	var photoStorage storage.Storage
	var err error
	switch cfg.Server.PhotoStorage.Backend {
	case "", "local":
		photoStorage, err = storage.NewLocalFS(cfg.PhotoDir())
	case "s3":
		photoStorage, err = storage.NewS3(cfg.Server.PhotoStorage.S3, nil)
		if err == nil {
			log.Info("photo storage: s3", "bucket", cfg.Server.PhotoStorage.S3.Bucket, "endpoint", cfg.Server.PhotoStorage.S3.Endpoint)
		}
	default:
		return nil, fmt.Errorf("server: photo storage backend %q not supported", cfg.Server.PhotoStorage.Backend)
	}
	if err != nil {
		return nil, fmt.Errorf("server: photo storage: %w", err)
	}

	userStore := users.NewStore(dbh)
	serverDir, _, _ := cfg.Paths()
	apiSrv, err := api.New(api.Deps{
		Cfg:          cfg,
		Log:          log,
		DB:           dbh,
		Users:        userStore,
		Sessions:     auth.NewManager(dbh, cfg.Server.SessionTTL),
		Photos:       photos.NewStore(dbh),
		Albums:       albums.NewStore(dbh),
		Devices:      devices.NewStore(dbh),
		Pairing:      pairing.NewStore(dbh),
		Releases:     releases.NewStore(dbh, filepath.Join(serverDir, "releases")),
		Storage:      photoStorage,
		Audit:        audit.NewRecorder(dbh, log),
		LoginLimiter: auth.NewRateLimiter(time.Minute, 1024),
		Pipeline:     photos.DefaultOptions(cfg.Server.MaxUploadBytes, cfg.Server.MaxDecodeDimension),
	})
	if err != nil {
		return nil, fmt.Errorf("server: api: %w", err)
	}

	return &Server{
		cfg:       cfg,
		log:       log,
		db:        dbh,
		api:       apiSrv,
		userStore: users.NewStore(dbh),
		start:     time.Now(),
	}, nil
}

// Router builds the HTTP handler tree (exposed separately for tests).
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(chimw.RequestID)
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			start := time.Now()
			next.ServeHTTP(w, req)
			s.log.Debug("http",
				"method", req.Method,
				"path", req.URL.Path,
				"request_id", chimw.GetReqID(req.Context()),
				"duration_ms", time.Since(start).Milliseconds(),
			)
		})
	})

	r.Get("/healthz", s.handleHealth)

	// REST API (M2+). Mounted on the /api prefix with its own routing,
	// auth, CSRF and JSON 404s.
	r.Mount("/api", s.api.Router())

	// Admin SPA (embedded at build time). The catch-all serves static
	// assets, falls back to index.html for extensionless SPA routes and
	// 404s everything else.
	r.Get("/*", web.SPAHandler())
	r.NotFound(web.NotFoundHandler())

	return r
}

// Health is the /healthz payload.
type Health struct {
	Status     string   `json:"status"` // ok | degraded
	Version    string   `json:"version"`
	Commit     string   `json:"commit"`
	StartedAt  string   `json:"started_at"`
	UptimeSecs int64    `json:"uptime_seconds"`
	Time       string   `json:"time"`
	DB         DBHealth `json:"db"`
}

type DBHealth struct {
	Status string `json:"status"` // ok | error
	Detail string `json:"detail,omitempty"`
}

// CheckHealth evaluates health without going through HTTP (used by the
// handler and by future watchdog integrations).
func (s *Server) CheckHealth(ctx context.Context) Health {
	h := Health{
		Status:    "ok",
		Version:   version.Version,
		Commit:    version.Commit,
		StartedAt: s.start.UTC().Format(time.RFC3339),
		Time:      time.Now().UTC().Format(time.RFC3339),
		DB:        DBHealth{Status: "ok"},
	}
	dbCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := PingDB(dbCtx, s.db); err != nil {
		h.Status = "degraded"
		h.DB = DBHealth{Status: "error", Detail: err.Error()}
	}
	h.UptimeSecs = int64(time.Since(s.start).Seconds())
	return h
}

func (s *Server) handleHealth(w http.ResponseWriter, req *http.Request) {
	h := s.CheckHealth(req.Context())
	statusCode := http.StatusOK
	if h.Status != "ok" {
		statusCode = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(h)
}

// ListenAddr returns the configured listen address.
func (s *Server) ListenAddr() string { return s.cfg.Server.Listen }

// bootstrapFirstAdmin implements the easy-first-run promise (spec §2):
// on a brand-new installation (no active users), an initial
// administrator is created with a generated temporary password and the
// credential is logged exactly once at Info level.
//
// This is the single documented exception to the "no secrets in logs"
// rule (ADR-0010): the value is a one-time recovery credential for an
// account that does not yet exist and must be changed. It is logged
// once, attributed to this server, and never repeated (subsequent
// startups take the no-op path).
func (s *Server) bootstrapFirstAdmin(ctx context.Context) error {
	n, err := s.userStore.CountActive(ctx)
	if err != nil {
		return fmt.Errorf("count users: %w", err)
	}
	if n > 0 {
		return nil
	}
	tmp, err := auth.TemporaryPassword()
	if err != nil {
		return fmt.Errorf("generate temporary password: %w", err)
	}
	u, err := s.userStore.Create(ctx, "admin@localhost", "Administrator", users.RoleAdmin, tmp)
	if err != nil {
		return fmt.Errorf("create initial admin: %w", err)
	}
	s.log.Info("first run: created initial administrator (one-time temporary password — change it after login)",
		"first_run", true,
		"email", u.Email,
		"temporary_password", tmp)
	return nil
}

// Run starts the HTTP server and blocks until ctx is cancelled, then
// performs a graceful shutdown bounded by server.shutdown_timeout.
//
// Readiness ordering (matters for systemd): the listener is created
// before Run returns its "started" log line, so a Type=notify unit can
// gate on the log/UDP notification without accepting half-configured
// traffic.
func (s *Server) Run(ctx context.Context) error {
	if err := s.bootstrapFirstAdmin(ctx); err != nil {
		s.log.Error("first-run bootstrap failed (continuing without it)", "error", err)
	}
	s.httpS = &http.Server{
		Addr:         s.cfg.Server.Listen,
		Handler:      s.Router(),
		ReadTimeout:  s.cfg.Server.ReadTimeout,
		WriteTimeout: s.cfg.Server.WriteTimeout,
		// IdleTimeout slightly above the keep-alive default so long
		// admin sessions are not dropped.
		IdleTimeout: s.cfg.Server.WriteTimeout + 10*time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	ln, err := net.Listen("tcp", s.cfg.Server.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.cfg.Server.Listen, err)
	}
	s.log.Info("server listening", "addr", ln.Addr().String(), "version", version.Version)

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.httpS.Serve(ln)
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve: %w", err)
		}
		return nil
	case <-ctx.Done():
	}

	s.log.Info("shutting down: draining connections",
		"timeout", s.cfg.Server.ShutdownTimeout.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.Server.ShutdownTimeout)
	defer cancel()
	if err := s.httpS.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	s.log.Info("server stopped")
	return nil
}

// PingDB probes database liveness with a bounded context.
func PingDB(ctx context.Context, dbh *sql.DB) error {
	return dbh.PingContext(ctx)
}
