package api

import (
	"database/sql"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/matthewgall/openframe/internal/config"
	"github.com/matthewgall/openframe/internal/server/albums"
	"github.com/matthewgall/openframe/internal/server/audit"
	"github.com/matthewgall/openframe/internal/server/auth"
	"github.com/matthewgall/openframe/internal/server/devices"
	"github.com/matthewgall/openframe/internal/server/pairing"
	"github.com/matthewgall/openframe/internal/server/photos"
	"github.com/matthewgall/openframe/internal/server/storage"
	"github.com/matthewgall/openframe/internal/server/users"
)

// sessionCookieName is the HTTP cookie carrying the session token.
const sessionCookieName = "of_session"

// protocolAuthLoginPath is the full login path (csrf exemption key).
const protocolAuthLoginPath = "/api/v1/auth/login"

// Server holds the API dependencies.
type Server struct {
	cfg          *config.Config
	log          *slog.Logger
	db           *sql.DB
	userStore    *users.Store
	sessionMgr   *auth.Manager
	photoStore   *photos.Store
	albumStore   *albums.Store
	deviceStore  *devices.Store
	pairingStore *pairing.Store
	store        storage.Storage
	auditRec     *audit.Recorder
	loginLimiter *auth.RateLimiter
	pipeOpts     photos.Options
}

// Deps bundles the constructed dependencies (built by internal/server).
type Deps struct {
	Cfg          *config.Config
	Log          *slog.Logger
	DB           *sql.DB
	Users        *users.Store
	Sessions     *auth.Manager
	Photos       *photos.Store
	Albums       *albums.Store
	Devices      *devices.Store
	Pairing      *pairing.Store
	Storage      storage.Storage
	Audit        *audit.Recorder
	LoginLimiter *auth.RateLimiter
	Pipeline     photos.Options
}

// New builds the API server.
func New(d Deps) (*Server, error) {
	return &Server{
		cfg:          d.Cfg,
		log:          d.Log,
		db:           d.DB,
		userStore:    d.Users,
		sessionMgr:   d.Sessions,
		photoStore:   d.Photos,
		albumStore:   d.Albums,
		deviceStore:  d.Devices,
		pairingStore: d.Pairing,
		store:        d.Storage,
		auditRec:     d.Audit,
		loginLimiter: d.LoginLimiter,
		pipeOpts:     d.Pipeline,
	}, nil
}

// Router builds the /api router (mounted by internal/server at /api).
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()

	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such api endpoint")
	})

	r.Route("/v1", func(v1 chi.Router) {
		// Public (auth-free) routes.
		v1.Post("/auth/login", s.handleLogin)
		v1.With(s.requireAuth).Get("/auth/csrf", s.handleCSRF)

		// Pairing protocol (M5, spec §12): frame-facing and public by
		// necessity (an unpaired frame has no credential); creation is
		// rate-limited per source address in the pairing store.
		v1.Post("/devices/pair-challenges", s.handlePairChallenge)
		v1.Post("/devices/pair-status", s.handlePairStatus)

		// Frame synchronisation API (M3, spec §9): authenticated by
		// device credential (Authorization: Bearer <token>), not by the
		// admin session. Frames initiate all requests; the server never
		// connects out to them (spec §12).
		v1.With(s.requireDevice).Get("/frames/{deviceId}/manifest", s.handleFrameManifest)
		v1.With(s.requireDevice).Get("/frames/{deviceId}/photos/{resource}", s.handleFramePhoto)
		v1.With(s.requireDevice).Post("/frames/{deviceId}/changes", s.handleFrameChanges)
		v1.With(s.requireDevice).Post("/frames/{deviceId}/health", s.handleFrameHealth)

		// Authenticated group: every route below requires a valid
		// session; mutating requests additionally require the CSRF
		// token (synchronizer token bound to the session).
		v1.Group(func(g chi.Router) {
			g.Use(s.requireAuth, s.csrfProtect)

			g.Post("/auth/logout", s.handleLogout)
			g.Get("/auth/me", s.handleMe)
			g.Post("/auth/password", s.handleChangePassword)

			g.With(requireAdmin).Get("/users", s.handleListUsers)
			g.With(requireAdmin).Post("/users", s.handleCreateUser)
			g.With(requireAdmin).Patch("/users/{id}", s.handleUpdateUser)
			g.With(requireAdmin).Post("/users/{id}/password-reset", s.handleResetPassword)
			g.With(requireAdmin).Post("/users/{id}/sessions/revoke", s.handleRevokeSessions)

			g.Get("/photos", s.handleListPhotos)
			g.Post("/photos/upload", s.handleUpload)
			g.Get("/photos/{id}", s.handleGetPhoto)
			g.Patch("/photos/{id}", s.handleUpdatePhoto)
			g.Delete("/photos/{id}", s.handleDeletePhoto)
			g.Post("/photos/{id}/restore", s.handleRestorePhoto)
			g.Get("/photos/{id}/file", s.handlePhotoFile)

			g.Get("/albums", s.handleListAlbums)
			g.Post("/albums", s.handleCreateAlbum)
			g.Get("/albums/{id}", s.handleGetAlbum)
			g.Patch("/albums/{id}", s.handleUpdateAlbum)
			g.Delete("/albums/{id}", s.handleDeleteAlbum)
			g.Post("/albums/{id}/restore", s.handleRestoreAlbum)
			g.Post("/albums/{id}/photos", s.handleAlbumAddPhoto)
			g.Delete("/albums/{id}/photos/{photoId}", s.handleAlbumRemovePhoto)
			g.Post("/albums/{id}/cover", s.handleAlbumSetCover)
			g.With(requireAdmin).Post("/albums/{id}/frames/{deviceId}", s.handleAlbumAssignDevice)
			g.With(requireAdmin).Delete("/albums/{id}/frames/{deviceId}", s.handleAlbumUnassignDevice)

			g.With(requireAdmin).Get("/devices", s.handleListDevices)
			g.With(requireAdmin).Post("/devices", s.handleCreateDevice)
			g.With(requireAdmin).Get("/devices/{id}", s.handleDeviceDetail)
			g.With(requireAdmin).Patch("/devices/{id}", s.handleUpdateDevice)
			g.With(requireAdmin).Post("/devices/{id}/revoke", s.handleRevokeDevice)
			g.With(requireAdmin).Post("/devices/{id}/credential", s.handleIssueDeviceCredential)
			g.With(requireAdmin).Delete("/devices/{id}/credential", s.handleRevokeDeviceCredential)
			g.With(requireAdmin).Get("/devices/{id}/config", s.handleDeviceConfig)
			g.With(requireAdmin).Put("/devices/{id}/config", s.handleDeviceConfig)
			g.With(requireAdmin).Post("/devices/{id}/sync", s.handleDeviceSync)

			g.With(requireAdmin).Get("/pairing/pending", s.handleListPairing)
			g.With(requireAdmin).Post("/pairing/{id}/approve", s.handleApprovePairing)
			g.With(requireAdmin).Post("/pairing/{id}/reject", s.handleRejectPairing)
			g.With(requireAdmin).Post("/photos/{id}/priority", s.handleSetPhotoPriority)
			g.With(requireAdmin).Post("/albums/{id}/priority", s.handleSetAlbumPriority)

			g.With(requireAdmin).Get("/audit", s.handleAudit)
			g.Get("/stats", s.handleStats)
		})
	})

	return r
}

// setSessionCookie writes (or clears, with zero TTL) the session cookie.
func (s *Server) setSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	secure := s.cfg.Server.SecureURL()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		Domain:   "",
		MaxAge:   int(time.Until(expires).Seconds()),
		Expires:  expires,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.cfg.Server.SecureURL(),
	})
}

// audit is a convenience wrapper (actor from context when present).
func (s *Server) audit(r *http.Request, action, targetType, targetID string, detail any) {
	actor := ""
	if u, ok := UserFrom(r.Context()); ok {
		actor = u.ID
	}
	s.auditRec.Action(r.Context(), actor, action, targetType, targetID, clientIP(r), detail)
}
