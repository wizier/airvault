// Package handler is AirVault's HTTP layer: a JSON API under /api and the
// embedded Svelte SPA served at the root. Built on Echo v5.
package handler

import (
	"io/fs"
	"net/http"
	"strings"

	"github.com/wizier/airvault/internal/auth"
	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/events"
	"github.com/wizier/airvault/internal/service"

	"github.com/labstack/echo/v5"
	echoMiddleware "github.com/labstack/echo/v5/middleware"
)

// maxIPABytes caps a user-uploaded .ipa (device apps are well under this).
const maxIPABytes = 2 << 30 // 2 GiB

// Handler owns the HTTP surface's dependencies. All data access goes through
// the Service (this layer only maps HTTP <-> use-cases); the bus feeds the SSE
// stream.
type Handler struct {
	auth     *auth.Credentials
	svc      *service.Service
	bus      *events.Bus
	staticFS fs.FS
}

// New builds the HTTP transport. Background-operation lifetime belongs to the service.
func New(credentials *auth.Credentials, svc *service.Service,
	bus *events.Bus, staticFS fs.FS) *Handler {
	return &Handler{auth: credentials, svc: svc, bus: bus, staticFS: staticFS}
}

// Router builds the Echo instance with middleware, the JSON API, and the SPA.
func (h *Handler) Router() *echo.Echo {
	e := echo.New()
	e.HTTPErrorHandler = h.errorHandler

	e.Use(echoMiddleware.Recover())
	// The SPA shell is public; /api is gated by apiAuthMiddleware on its group.
	// CSRF stays global so the shell load seeds the token before the login POST.
	e.Use(csrfMiddleware())
	// 1 MB is plenty for the JSON API; the .ipa upload streams a big file, so it
	// opts out here and carries its own larger route-level limit.
	e.Use(echoMiddleware.BodyLimitWithConfig(echoMiddleware.BodyLimitConfig{
		LimitBytes: 1 << 20,
		Skipper: func(c *echo.Context) bool {
			return strings.HasSuffix(c.Request().URL.Path, "/apps/install")
		},
	}))
	e.Use(echoMiddleware.SecureWithConfig(echoMiddleware.SecureConfig{
		XFrameOptions:      "DENY",
		ContentTypeNosniff: "nosniff",
		ReferrerPolicy:     "same-origin",
	}))
	e.Use(echoMiddleware.GzipWithConfig(echoMiddleware.GzipConfig{
		// SSE and the install progress stream must flush immediately; downloads,
		// previews and wallpapers carry already compressed media (JPEG/HEIC/PNG) with
		// an exact Content-Length — re-compressing them only wastes CPU and voids the length.
		Skipper: func(c *echo.Context) bool {
			p := c.Request().URL.Path
			return p == "/api/events" || strings.HasSuffix(p, "/console") ||
				strings.HasSuffix(p, "/apps/install") ||
				strings.HasSuffix(p, "/download") || strings.HasSuffix(p, "/preview") ||
				strings.HasSuffix(p, "/wallpaper")
		},
	}))

	// Liveness for uptime monitors — DB reachability only.
	e.GET("/healthz", func(c *echo.Context) error {
		if err := h.svc.Ping(c.Request().Context()); err != nil {
			return c.String(http.StatusServiceUnavailable, "db unavailable")
		}
		return c.String(http.StatusOK, "ok")
	})

	// Login session (token -> HttpOnly cookie). Open — it validates the token
	// itself; everything under the /api group requires a session or Basic auth.
	e.POST("/api/session", h.createSession)
	e.DELETE("/api/session", h.deleteSession)

	api := e.Group("/api", apiAuthMiddleware(h.auth))

	// System and real-time state. The build version is baked into the SPA at
	// build time (VITE_APP_VERSION), so it needs no endpoint.
	api.GET("/status", h.status)
	api.GET("/devices", h.listDevices)
	api.GET("/events", h.streamEvents)

	// Runtime control for in-flight backup/restore runs.
	api.POST("/runs/:id/cancel", h.cancelRun)

	// Backup catalog and restore selection.
	api.GET("/restore-sources", h.listRestoreSources)

	// Guided pairing wizard — pairing ONLY; backups and encryption are
	// configured later on the device page (a paired-but-never-backed-up phone
	// is a valid end state). Pairing always enables Wi-Fi sync.
	api.GET("/pair/state", h.getPairingState)
	api.POST("/pair/trust", h.startPairing)

	// Everything below this group targets one registered device.
	device := api.Group("/devices/:udid", udidGuard)
	device.GET("/backups", h.listBackups)
	device.POST("/backup", h.startBackup)
	device.DELETE("/backups", h.deleteBackups)
	device.GET("/backups/reclaimable", h.snapshotsReclaimable)
	device.POST("/restore", h.startRestore)
	device.DELETE("/pairing", h.unpairDevice)
	device.POST("/backup-password", h.changeBackupPassword)
	device.POST("/power", h.controlPower)
	device.GET("/battery", h.getDeviceBattery)
	device.GET("/hardware", h.getHardware)
	device.GET("/wallpaper", h.getWallpaper)
	device.GET("/apps", h.listApps)
	device.POST("/apps/install", h.installApp, echoMiddleware.BodyLimit(maxIPABytes))
	device.POST("/apps/icons", h.appIcons)
	device.DELETE("/apps/:bundle", h.uninstallApp)
	device.GET("/apps/:bundle/files", h.listAppFiles)
	device.DELETE("/apps/:bundle/files", h.deleteAppFile)
	device.GET("/apps/:bundle/files/stat", h.appFileStat)
	device.GET("/apps/:bundle/files/download", h.downloadAppFile)
	device.GET("/apps/:bundle/files/preview", h.previewAppFile)
	device.GET("/media", h.listMedia)
	device.GET("/media/download", h.downloadMedia)
	device.GET("/media/preview", h.previewMedia)
	device.GET("/media/gallery", h.galleryList)
	device.POST("/media/thumbs", h.mediaThumbs)
	device.GET("/media/stat", h.mediaStat)
	device.GET("/console", h.streamDeviceConsole)

	// The embedded Svelte SPA at the root (/api and /healthz are more specific
	// and always win). In dev you browse Vite at :8080 instead — it proxies the
	// API to the daemon at :8081 (see web/vite.config.ts and `make dev`).
	e.GET("/*", echo.WrapHandler(http.FileServer(http.FS(h.staticFS))))
	return e
}

// udidGuard rejects :udid values that could not possibly be a device id —
// udids feed filesystem paths downstream, so this is cheap defense-in-depth
// in front of the DB-existence checks every operation already performs.
func udidGuard(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if domain.ValidateSource(c.Param("udid")) != nil {
			return domain.ErrNotFound
		}
		return next(c)
	}
}
