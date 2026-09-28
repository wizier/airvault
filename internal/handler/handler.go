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

const maxIPABytes = 2 << 30 // 2 GiB

type Handler struct {
	auth     *auth.Credentials
	svc      *service.Service
	bus      *events.Bus
	staticFS fs.FS
}

func New(credentials *auth.Credentials, svc *service.Service,
	bus *events.Bus, staticFS fs.FS) *Handler {
	return &Handler{auth: credentials, svc: svc, bus: bus, staticFS: staticFS}
}

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
		// previews and wallpapers carry an exact Content-Length and Range that
		// compression would void.
		Skipper: func(c *echo.Context) bool {
			p := c.Request().URL.Path
			return p == "/api/events" || strings.HasSuffix(p, "/console") ||
				strings.HasSuffix(p, "/apps/install") ||
				strings.HasSuffix(p, "/download") || strings.HasSuffix(p, "/preview") ||
				strings.HasSuffix(p, "/wallpaper")
		},
	}))

	e.GET("/healthz", func(c *echo.Context) error {
		if err := h.svc.Ping(c.Request().Context()); err != nil {
			return c.String(http.StatusServiceUnavailable, "db unavailable")
		}
		return c.String(http.StatusOK, "ok")
	})

	// Outside the auth group: it validates the token itself.
	e.POST("/api/session", h.createSession)
	e.DELETE("/api/session", h.deleteSession)

	api := e.Group("/api", apiAuthMiddleware(h.auth))

	api.GET("/status", h.status)
	api.GET("/devices", h.listDevices)
	api.GET("/events", h.streamEvents)

	api.POST("/runs/:id/cancel", h.cancelRun)

	api.GET("/restore-sources", h.listRestoreSources)
	api.GET("/backups/:snapshotId/download", h.downloadBackup)
	api.HEAD("/backups/:snapshotId/download", h.downloadBackup)

	api.GET("/pair/state", h.getPairingState)
	api.POST("/pair/trust", h.startPairing)

	device := api.Group("/devices/:udid", udidGuard)
	device.GET("/backups", h.listBackups)
	device.POST("/backup", h.startBackup)
	device.DELETE("/backups", h.deleteBackups)
	device.GET("/backups/reclaimable", h.snapshotsReclaimable)
	device.POST("/restore", h.startRestore)
	device.DELETE("/pairing", h.unpairDevice)
	device.POST("/backup-password", h.changeBackupPassword)
	device.PUT("/auto-backup", h.setAutoBackup)
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

	e.GET("/*", echo.WrapHandler(http.FileServer(http.FS(h.staticFS))))
	return e
}

// UDIDs feed filesystem paths downstream, so this is cheap defense-in-depth in
// front of the DB-existence checks every operation already performs.
func udidGuard(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if domain.ValidateSource(c.Param("udid")) != nil {
			return domain.ErrNotFound
		}
		return next(c)
	}
}
