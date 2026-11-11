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
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/matthewgall/openframe/internal/config"
	"github.com/matthewgall/openframe/internal/version"
	"github.com/matthewgall/openframe/web"
)

// Server is the management server instance.
type Server struct {
	cfg   *config.Config
	log   *slog.Logger
	db    *sql.DB
	httpS *http.Server
	start time.Time
}

// New builds a Server. The db must already be migrated.
func New(cfg *config.Config, log *slog.Logger, dbh *sql.DB) (*Server, error) {
	if cfg == nil {
		return nil, fmt.Errorf("server: nil config")
	}
	if dbh == nil {
		return nil, fmt.Errorf("server: nil db")
	}
	return &Server{
		cfg:   cfg,
		log:   log,
		db:    dbh,
		start: time.Now(),
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

// Run starts the HTTP server and blocks until ctx is cancelled, then
// performs a graceful shutdown bounded by server.shutdown_timeout.
//
// Readiness ordering (matters for systemd): the listener is created
// before Run returns its "started" log line, so a Type=notify unit can
// gate on the log/UDP notification without accepting half-configured
// traffic.
func (s *Server) Run(ctx context.Context) error {
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
