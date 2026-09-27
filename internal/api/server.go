package api

import (
	"context"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
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
	Config         *config.Config
	TrustedProxies *TrustedProxies // proxies whose forwarding headers identify the client; nil trusts none
	DB             *database.DB
	MQTT           *mqttclient.Client
	Live           LiveDataSource
	Uploader       CallUploader       // nil if upload ingest not available
	AudioStreamer  AudioStreamer      // nil if live audio streaming not configured
	Store          storage.AudioStore // audio storage backend (local, S3, or tiered)
	WebFiles       fs.FS              // embedded web/ directory
	OpenAPISpec    []byte             // embedded openapi.yaml
	Version        string
	StartTime      time.Time
	Log            zerolog.Logger
	OnSystemMerge  func(sourceID, targetID int) // called after successful system merge to invalidate caches
	TGCSVPaths     map[int]string               // system_id → CSV file path for talkgroup writeback
	UnitCSVPaths   map[int]string               // system_id → CSV file path for unit tag writeback

	// Update checker (opt-in)
	UpdateCheckURL string // base URL for version check API
	IngestModes    string // comma-separated active ingest modes
	IsDocker       bool   // running inside Docker container
}

func NewServer(opts ServerOptions) *Server {
	// Prometheus collectors are registered here, once, rather than in
	// buildRouter, so the router can be built more than once (tests).
	if opts.Config.MetricsEnabled {
		var ingestStats metrics.IngestStats
		if opts.Live != nil {
			ingestStats = &liveDataMetricsAdapter{live: opts.Live}
		}
		prometheus.MustRegister(metrics.NewCollector(opts.DB.Pool, ingestStats))
	}

	health := NewHealthHandler(opts.DB, opts.MQTT, opts.Live, opts.AudioStreamer, opts.Version, opts.StartTime)
	if opts.UpdateCheckURL != "" {
		health.ConfigureUpdateChecker(opts.UpdateCheckURL, opts.IngestModes, opts.IsDocker, opts.Log)
	}

	r := buildRouter(routerOptions{
		ServerOptions: opts,
		authn: newAuthenticator(opts.DB, opts.TrustedProxies,
			opts.Config.RateLimitRPS, opts.Config.RateLimitBurst, opts.Log),
		health: health,
	})

	srv := &http.Server{
		Addr:        opts.Config.HTTPAddr,
		Handler:     r,
		ReadTimeout: opts.Config.ReadTimeout,
		IdleTimeout: opts.Config.IdleTimeout,
		// WriteTimeout set to 0 to allow long-lived SSE connections.
		// Individual non-streaming handlers complete quickly due to DB query timeouts.
		WriteTimeout: 0,
		// The request line and headers: generous for long filter query
		// strings and tickets, far below the 1 MB default.
		MaxHeaderBytes: maxHeaderBytes,
	}

	return &Server{
		http:   srv,
		log:    opts.Log,
		health: health,
	}
}

// maxHeaderBytes limits the request line plus headers (http.Server).
const maxHeaderBytes = 64 << 10

// routerOptions are buildRouter's dependencies.
type routerOptions struct {
	ServerOptions
	authn  *authenticator // principal resolution, authorization and audit
	health *HealthHandler
}

// buildRouter builds the root router: the §6.2 pipeline and every route,
// registered flat (full patterns, no sub-routers) so that chi.Walk and
// Mux.Find agree on the pattern strings the policy table uses. Conditional
// routes (metrics, upload, live audio) are registered when their dependency
// is set, so a build with stub dependencies has every route.
func buildRouter(opts routerOptions) *chi.Mux {
	cfg := opts.Config
	a := opts.authn
	r := chi.NewRouter()

	r.Use(RequestID)
	r.Use(CORS)
	r.Use(middleware.GetHead)
	// Recoverer inside Logger: the panic is logged with the request's logger
	// and the access line records the 500.
	r.Use(Logger(opts.Log))
	r.Use(Recoverer)
	r.Use(APIHeaders)
	r.Use(Match(r, opts.Log))
	r.Use(a.Resolve)
	r.Use(a.Authorize)
	r.Use(a.Audit)

	r.NotFound(notFound)
	r.MethodNotAllowed(methodNotAllowed(r))

	health := opts.health
	if health == nil {
		health = NewHealthHandler(opts.DB, opts.MQTT, opts.Live, opts.AudioStreamer, opts.Version, opts.StartTime)
	}
	r.Get("/api/v1/health", health.ServeHTTP)

	// Prometheus metrics (a key with listen; never public).
	if cfg.MetricsEnabled {
		r.Get("/metrics", promhttp.Handler().ServeHTTP)
	}

	// Call upload: the key may come from the multipart form (trunk-recorder's
	// upload plugins), read after the body limit.
	if opts.Uploader != nil {
		uploadHandler := NewUploadHandler(opts.Uploader, cfg.UploadInstanceID, opts.Log)
		r.Group(func(r chi.Router) {
			r.Use(MaxBodySize(50 << 20)) // 50 MB for audio uploads
			r.Use(a.UploadAuth)
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
	} else if opts.WebFiles != nil {
		webFSys, _ = fs.Sub(opts.WebFiles, "web")
	}
	if webFSys == nil {
		webFSys = emptyFS{}
	}

	r.Group(func(r chi.Router) {
		r.Use(MaxBodySize(10 << 20)) // 10 MB for regular API requests
		if cfg.MetricsEnabled {
			r.Use(metrics.InstrumentHandler)
		}
		r.Use(ResponseTimeout(cfg.WriteTimeout))

		api := prefixRouter{Router: r, prefix: apiPrefix}

		// Auth: who am I, keys, anonymous access, tickets, audit log.
		NewAccessHandler(opts.DB, a, opts.Version).Routes(api)
		NewKeysHandler(opts.DB, a).Routes(api)
		NewTicketsHandler(a).Routes(api)

		NewSystemsHandler(opts.DB).Routes(api)
		NewTalkgroupsHandler(opts.DB, opts.TGCSVPaths).Routes(api)
		NewUnitsHandler(opts.DB, opts.UnitCSVPaths).Routes(api)
		NewUnitTagSuggestionsHandler(opts.DB, opts.UnitCSVPaths, cfg.UnitTagSuggestions,
			cfg.UnitTagSuggestionsMinCalls, cfg.UnitTagSuggestionsMinShare).Routes(api)
		NewCallsHandler(opts.DB, cfg.AudioDir, cfg.TRAudioDir, opts.Store, opts.Live).Routes(api)
		NewCallGroupsHandler(opts.DB, cfg.TRAudioDir).Routes(api)
		NewStatsHandler(opts.DB).Routes(api)
		NewRecordersHandler(opts.Live).Routes(api)
		NewEventsHandler(opts.Live).Routes(api)
		if opts.AudioStreamer != nil {
			NewAudioStreamHandler(opts.AudioStreamer, cfg.StreamMaxClients).Routes(api)
		}
		NewUnitEventsHandler(opts.DB).Routes(api)
		NewAffiliationsHandler(opts.Live).Routes(api)
		NewTranscriptionsHandler(opts.DB, opts.Live).Routes(api)
		NewAdminHandler(opts.DB, opts.Live, opts.OnSystemMerge).Routes(api)
		NewStorageHandler(opts.DB, opts.Log).Routes(api)
		NewQueryHandler(opts.DB).Routes(api)
		api.Post("/pages", SavePageHandler(webDir))

		// Debug report (admin): sends server diagnostics to DEBUG_REPORT_URL.
		// Always registered; the handler returns 503 when disabled via
		// DEBUG_REPORT_DISABLE=true.
		// A nil *mqttclient.Client (no MQTT_BROKER_URL) must be a nil
		// MQTTStatus, not a non-nil interface holding a nil pointer.
		var mqttStatus MQTTStatus
		if opts.MQTT != nil {
			mqttStatus = opts.MQTT
		}
		debugReport := NewDebugReportHandler(DebugReportOptions{
			DB:            opts.DB,
			Config:        cfg,
			Live:          opts.Live,
			AudioStreamer: opts.AudioStreamer,
			MQTT:          mqttStatus,
			Log:           opts.Log,
			Version:       opts.Version,
			StartTime:     opts.StartTime,
		})
		api.Post("/debug-report", debugReport.Submit)
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
	r.Get("/*", staticHandler(webFSys))

	return r
}

// staticHandler serves the web pages. Paths under /api/v1 that reach it
// matched no API route (e.g. the removed /auth-init) and get a JSON 404.
func staticHandler(webFSys fs.FS) http.HandlerFunc {
	files := http.FileServer(http.FS(webFSys))
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == apiPrefix || strings.HasPrefix(r.URL.Path, apiPrefix+"/") {
			notFound(w, r)
			return
		}
		files.ServeHTTP(w, r)
	}
}

func notFound(w http.ResponseWriter, r *http.Request) {
	WriteErrorWithCode(w, http.StatusNotFound, ErrNotFound, "not found")
}

// methodNotAllowed answers a path that exists only for other methods. A
// path that only the static catch-all (GET /*) matches doesn't exist, so it
// gets 404 (e.g. the removed POST /auth/login).
func methodNotAllowed(root *chi.Mux) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := findRoute(root, http.MethodGet, routingPath(r)); pattern == "/*" {
			notFound(w, r)
			return
		}
		WriteErrorWithCode(w, http.StatusMethodNotAllowed, ErrBadRequest, "method not allowed")
	}
}

// emptyFS is a web root with no files, for routers built without web files.
type emptyFS struct{}

func (emptyFS) Open(name string) (fs.File, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// prefixRouter registers routes flat on the underlying router with a fixed
// prefix, so handler Routes methods keep their relative patterns while chi
// sees full ones. Sub-routers (Route, Mount) and method-less registrations
// (Handle, HandleFunc) are refused: every route needs its own entry in the
// policy table.
type prefixRouter struct {
	chi.Router
	prefix string
}

func (p prefixRouter) With(mw ...func(http.Handler) http.Handler) chi.Router {
	return prefixRouter{Router: p.Router.With(mw...), prefix: p.prefix}
}

func (p prefixRouter) Group(fn func(r chi.Router)) chi.Router {
	return p.Router.Group(func(r chi.Router) { fn(prefixRouter{Router: r, prefix: p.prefix}) })
}

func (p prefixRouter) Route(pattern string, fn func(r chi.Router)) chi.Router {
	panic("api: register routes flat (no Route) so the policy table matches chi.Find: " + p.prefix + pattern)
}

func (p prefixRouter) Mount(pattern string, h http.Handler) {
	panic("api: register routes flat (no Mount) so the policy table matches chi.Find: " + p.prefix + pattern)
}

func (p prefixRouter) Handle(pattern string, h http.Handler) {
	panic("api: register routes with an explicit method: " + p.prefix + pattern)
}

func (p prefixRouter) HandleFunc(pattern string, h http.HandlerFunc) {
	panic("api: register routes with an explicit method: " + p.prefix + pattern)
}

func (p prefixRouter) Method(method, pattern string, h http.Handler) {
	p.Router.Method(method, p.prefix+pattern, h)
}

func (p prefixRouter) MethodFunc(method, pattern string, h http.HandlerFunc) {
	p.Router.MethodFunc(method, p.prefix+pattern, h)
}

func (p prefixRouter) Connect(pattern string, h http.HandlerFunc) {
	p.Router.Connect(p.prefix+pattern, h)
}
func (p prefixRouter) Delete(pattern string, h http.HandlerFunc) {
	p.Router.Delete(p.prefix+pattern, h)
}
func (p prefixRouter) Get(pattern string, h http.HandlerFunc)  { p.Router.Get(p.prefix+pattern, h) }
func (p prefixRouter) Head(pattern string, h http.HandlerFunc) { p.Router.Head(p.prefix+pattern, h) }
func (p prefixRouter) Options(pattern string, h http.HandlerFunc) {
	p.Router.Options(p.prefix+pattern, h)
}
func (p prefixRouter) Patch(pattern string, h http.HandlerFunc) { p.Router.Patch(p.prefix+pattern, h) }
func (p prefixRouter) Post(pattern string, h http.HandlerFunc)  { p.Router.Post(p.prefix+pattern, h) }
func (p prefixRouter) Put(pattern string, h http.HandlerFunc)   { p.Router.Put(p.prefix+pattern, h) }
func (p prefixRouter) Trace(pattern string, h http.HandlerFunc) { p.Router.Trace(p.prefix+pattern, h) }

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
