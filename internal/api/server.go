package api

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/config"
	"github.com/snarg/tr-engine/internal/database"
	"github.com/snarg/tr-engine/internal/metrics"
	"github.com/snarg/tr-engine/internal/mqttclient"
	"github.com/snarg/tr-engine/internal/storage"
)

type Server struct {
	http   *http.Server
	log    zerolog.Logger
	health *HealthHandler
}

type ServerOptions struct {
	Config        *config.Config
	DB            *database.DB
	MQTT          *mqttclient.Client
	Live          LiveDataSource
	Uploader      CallUploader      // nil if upload ingest not available
	AudioStreamer AudioStreamer      // nil if live audio streaming not configured
	Store         storage.AudioStore // audio storage backend (local, S3, or tiered)
	WebFiles      fs.FS              // embedded web/ directory
	OpenAPISpec   []byte       // embedded openapi.yaml
	Version       string
	StartTime     time.Time
	Log           zerolog.Logger
	OnSystemMerge func(sourceID, targetID int) // called after successful system merge to invalidate caches
	TGCSVPaths    map[int]string               // system_id → CSV file path for talkgroup writeback
	UnitCSVPaths  map[int]string               // system_id → CSV file path for unit tag writeback

	// Update checker (opt-in)
	UpdateCheckURL string // base URL for version check API
	IngestModes    string // comma-separated active ingest modes
	IsDocker       bool   // running inside Docker container
}

func NewServer(opts ServerOptions) *Server {
	r := chi.NewRouter()

	// Parse CORS origins from config
	var corsOrigins []string
	if opts.Config.CORSOrigins != "" {
		for _, o := range strings.Split(opts.Config.CORSOrigins, ",") {
			if s := strings.TrimSpace(o); s != "" {
				corsOrigins = append(corsOrigins, s)
			}
		}
	}

	// Global middleware (no MaxBodySize here — upload endpoint needs a larger limit)
	r.Use(RequestID)
	r.Use(CORSWithOrigins(corsOrigins))
	r.Use(RateLimiter(opts.Config.RateLimitRPS, opts.Config.RateLimitBurst))
	r.Use(Recoverer)
	r.Use(Logger(opts.Log))

	// Unauthenticated endpoints
	health := NewHealthHandler(opts.DB, opts.MQTT, opts.Live, opts.AudioStreamer, opts.Version, opts.StartTime)
	if opts.UpdateCheckURL != "" {
		health.ConfigureUpdateChecker(opts.UpdateCheckURL, opts.IngestModes, opts.IsDocker, opts.Log)
	}
	r.Get("/api/v1/health", health.ServeHTTP)

	// Prometheus metrics endpoint (unauthenticated, like /health)
	if opts.Config.MetricsEnabled {
		var ingestStats metrics.IngestStats
		if opts.Live != nil {
			ingestStats = &liveDataMetricsAdapter{live: opts.Live}
		}
		collector := metrics.NewCollector(opts.DB.Pool, ingestStats)
		prometheus.MustRegister(collector)
		r.Get("/metrics", promhttp.Handler().ServeHTTP)
	}

	// Debug report endpoint (no auth — consent shown on page)
	// Always registered; handler returns 503 if disabled via DEBUG_REPORT_DISABLE=true
	{
		debugReport := NewDebugReportHandler(DebugReportOptions{
			DB:           opts.DB,
			Config:       opts.Config,
			Live:         opts.Live,
			AudioStreamer: opts.AudioStreamer,
			MQTT:         opts.MQTT,
			Log:          opts.Log,
			Version:      opts.Version,
			StartTime:    opts.StartTime,
		})
		r.Post("/api/v1/debug-report", debugReport.Submit)
	}

	// Web auth bootstrap — returns the read token for web UI pages.
	// If a valid JWT is present in the request, also returns user info.
	if opts.Config.AuthToken != "" {
		jwtSecret := []byte(opts.Config.JWTSecret)
		r.Get("/api/v1/auth-init", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")

			resp := map[string]any{
				"token": opts.Config.AuthToken,
			}

			// If there's a valid JWT, include user info (backward compatible —
			// "user" field is absent, not null, when no JWT present)
			if provided := extractBearerToken(r); provided != "" && len(jwtSecret) > 0 && strings.Count(provided, ".") == 2 {
				claims := &Claims{}
				token, err := jwt.ParseWithClaims(provided, claims, jwtKeyFunc(jwtSecret))
				if err == nil && token.Valid {
					resp["user"] = map[string]any{
						"username": claims.Username,
						"role":     claims.Role,
					}
				}
			}

			b, _ := json.Marshal(resp)
			w.Write(b)
		})
	}

	// User auth endpoints — single AuthHandler instance shared across
	// unauthenticated routes (login/refresh/logout) and authenticated (/auth/me)
	authRateLimit := AuthRateLimiter()
	var authHandler *AuthHandler
	if opts.Config.JWTSecret != "" {
		authHandler = NewAuthHandler(opts.DB, []byte(opts.Config.JWTSecret), opts.Log)
		r.With(authRateLimit).Post("/api/v1/auth/login", authHandler.Login)
		r.Post("/api/v1/auth/refresh", authHandler.Refresh)
		r.Post("/api/v1/auth/logout", authHandler.Logout)
	}

	// First-run setup — unauthenticated, only works when 0 users exist.
	// Only registered when user auth is configured (JWT secret present).
	if opts.Config.JWTSecret != "" {
		setupHandler := NewSetupHandler(opts.DB, opts.Log)
		r.Get("/api/v1/auth/setup", setupHandler.Status)
		r.With(authRateLimit).Post("/api/v1/auth/setup", setupHandler.Setup)
	}

	// Upload endpoint with custom auth (accepts form field key/api_key)
	// Uploads are write operations — require WRITE_TOKEN when set.
	// When auth is enabled but WRITE_TOKEN is not set, uploads are blocked
	// (UploadAuth with empty token rejects all requests).
	if opts.Uploader != nil {
		uploadToken := opts.Config.WriteToken
		uploadHandler := NewUploadHandler(opts.Uploader, opts.Config.UploadInstanceID, opts.Log)
		r.Group(func(r chi.Router) {
			r.Use(MaxBodySize(50 << 20)) // 50 MB for audio uploads
			r.Use(UploadAuth(uploadToken))
			r.Post("/api/v1/call-upload", uploadHandler.Upload)
		})
	}

	// Detect web directory: prefer local web/ on disk for dev, fall back to embedded
	var webFSys fs.FS
	var webDir string
	if info, err := os.Stat("web"); err == nil && info.IsDir() && fileExists("web/index.html") {
		webFSys = os.DirFS("web")
		if abs, err := filepath.Abs("web"); err == nil {
			webDir = abs
		}
		opts.Log.Info().Msg("serving web files from disk (dev mode)")
	} else {
		webFSys, _ = fs.Sub(opts.WebFiles, "web")
	}

	// Authenticated routes
	r.Group(func(r chi.Router) {
		r.Use(MaxBodySize(10 << 20)) // 10 MB for regular API requests
		if opts.Config.MetricsEnabled {
			r.Use(metrics.InstrumentHandler)
		}
		if opts.Config.AuthEnabled {
			// Use JWTOrTokenAuth which handles JWT, API keys, and legacy token auth.
			r.Use(JWTOrTokenAuth([]byte(opts.Config.JWTSecret), opts.Config.WriteToken, opts.Config.AuthToken, opts.DB))

			r.Use(WriteAuth(opts.Config.WriteToken, opts.Config.AuthToken))
		}
		r.Use(ResponseTimeout(opts.Config.WriteTimeout))

		// All API routes under /api/v1
		r.Route("/api/v1", func(r chi.Router) {
			// Auth: /me requires authentication (handled by outer group)
			if authHandler != nil {
				r.Get("/auth/me", authHandler.Me)
			}

			NewSystemsHandler(opts.DB).Routes(r)
			NewTalkgroupsHandler(opts.DB, opts.TGCSVPaths).Routes(r)
			NewUnitsHandler(opts.DB, opts.UnitCSVPaths).Routes(r)
			NewCallsHandler(opts.DB, opts.Config.AudioDir, opts.Config.TRAudioDir, opts.Store, opts.Live).Routes(r)
			NewCallGroupsHandler(opts.DB, opts.Config.TRAudioDir).Routes(r)
			NewStatsHandler(opts.DB).Routes(r)
			NewRecordersHandler(opts.Live).Routes(r)
			NewEventsHandler(opts.Live).Routes(r)
			if opts.AudioStreamer != nil {
				NewAudioStreamHandler(opts.AudioStreamer, opts.Config.StreamMaxClients).Routes(r)
			}
			NewUnitEventsHandler(opts.DB).Routes(r)
			NewAffiliationsHandler(opts.Live).Routes(r)
			NewTranscriptionsHandler(opts.DB, opts.Live).Routes(r)
			NewAdminHandler(opts.DB, opts.Live, opts.OnSystemMerge).Routes(r)
			r.Post("/pages", SavePageHandler(webDir))

			NewQueryHandler(opts.DB).Routes(r)

			// API key management
			{
				keysHandler := NewKeysHandler(opts.DB, opts.Log)
				r.Route("/auth/keys", func(r chi.Router) {
					// Editor+ routes (own key management)
					r.Group(func(r chi.Router) {
						r.Use(EditorOrAbove)
						r.Get("/", keysHandler.ListOwn)
						r.Post("/", keysHandler.Create)
						r.Delete("/{id}", keysHandler.DeleteOwn)
					})
					// Admin-only routes
					r.Group(func(r chi.Router) {
						r.Use(AdminOnly)
						r.Get("/all", keysHandler.ListAll)
						r.Post("/service", keysHandler.CreateServiceAccount)
						r.Delete("/{id}/any", keysHandler.DeleteAny)
					})
				})
			}

			// User management (admin only)
			if opts.Config.JWTSecret != "" {
				r.Route("/users", func(r chi.Router) {
					r.Use(AdminOnly)
					NewUsersHandler(opts.DB, opts.Log).Routes(r)
				})
			}
		})
	})

	// Serve embedded OpenAPI spec
	r.Get("/api/v1/openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/yaml")
		w.Write(opts.OpenAPISpec)
	})

	// Favicon — simple SVG radio tower icon
	r.Get("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32"><rect width="32" height="32" rx="6" fill="#1a1a2e"/><path d="M16 8v16M12 24h8M10 12a8 8 0 0 1 12 0M7 9a12 12 0 0 1 18 0" stroke="#00d4ff" stroke-width="2" fill="none" stroke-linecap="round"/><circle cx="16" cy="8" r="2" fill="#00d4ff"/></svg>`))
	})

	r.Get("/api/v1/pages", PagesHandler(webFSys))
	r.Handle("/*", http.FileServer(http.FS(webFSys)))

	srv := &http.Server{
		Addr:        opts.Config.HTTPAddr,
		Handler:     r,
		ReadTimeout: opts.Config.ReadTimeout,
		IdleTimeout: opts.Config.IdleTimeout,
		// WriteTimeout set to 0 to allow long-lived SSE connections.
		// Individual non-streaming handlers complete quickly due to DB query timeouts.
		WriteTimeout: 0,
	}

	return &Server{
		http:   srv,
		log:    opts.Log,
		health: health,
	}
}

// StartUpdateChecker begins periodic update checks if configured.
func (s *Server) StartUpdateChecker(ctx context.Context) {
	s.health.StartUpdateChecker(ctx)
}

func (s *Server) Start() error {
	s.log.Info().Str("addr", s.http.Addr).Msg("http server starting")
	err := s.http.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.log.Info().Msg("http server shutting down")
	return s.http.Shutdown(ctx)
}

// liveDataMetricsAdapter adapts LiveDataSource to metrics.IngestStats.
type liveDataMetricsAdapter struct {
	live LiveDataSource
}

func (a *liveDataMetricsAdapter) MsgCount() int64 {
	m := a.live.IngestMetrics()
	if m == nil {
		return 0
	}
	return m.MsgCount
}

func (a *liveDataMetricsAdapter) HandlerCounts() map[string]int64 {
	m := a.live.IngestMetrics()
	if m == nil {
		return nil
	}
	return m.HandlerCounts
}

func (a *liveDataMetricsAdapter) ActiveCallCount() int {
	m := a.live.IngestMetrics()
	if m == nil {
		return 0
	}
	return m.ActiveCalls
}

func (a *liveDataMetricsAdapter) SSESubscriberCount() int {
	m := a.live.IngestMetrics()
	if m == nil {
		return 0
	}
	return m.SSESubscribers
}

// fileExists returns true if the path exists and is a regular file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

