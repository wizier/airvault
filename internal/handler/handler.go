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
	e.Use(writeDeadlines(writeTimeout))
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
		// compression would void, and photos are compressed already.
		Skipper: func(c *echo.Context) bool {
			p := c.Request().URL.Path
			return p == "/api/events" || strings.HasSuffix(p, "/console") ||
				strings.HasSuffix(p, "/apps/install") ||
				strings.HasSuffix(p, "/download") || strings.HasSuffix(p, "/preview") ||
				strings.HasSuffix(p, "/wallpaper") || strings.HasSuffix(p, "/photo")
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
	backup := api.Group("/backups/:snapshotId")
	backup.GET("/download", h.downloadBackup)
	backup.HEAD("/download", h.downloadBackup)
	backup.POST("/unlock", h.unlockBackup)
	backup.GET("/components", backupJSON("components", h.svc.BackupComponents))
	backup.GET("/contacts", backupJSON("contacts", h.svc.BackupContacts))
	backup.GET("/contacts/:contactId/photo", h.backupContactPhoto)
	backup.GET("/calls", backupJSON("calls", h.svc.BackupCalls))
	backup.GET("/notes", backupJSON("notes", h.svc.BackupNotes))
	backup.GET("/photos/gallery", h.backupPhotos)
	backup.GET("/photos/months", h.backupPhotoMonths)
	backup.GET("/photos/filters", backupJSON("filters", h.svc.BackupPhotoFilters))
	backup.POST("/photos/thumbs", h.backupPhotoThumbs)
	// The chats of messages and whatsapp; the files of every component.
	backup.GET("/:component/chats", h.backupChats)
	backup.GET("/:component/chats/messages", h.backupMessages)
	backup.GET("/:component/chats/matches", h.backupChatSearch)
	backup.GET("/:component/matches", h.backupSearch)
	backup.GET("/:component/files/stat", serveFile(h.openBackupFile, serveStat))
	backup.GET("/:component/files/download", serveFile(h.openBackupFile, serveDownload))
	backup.GET("/:component/files/preview", serveFile(h.openBackupFile, streamPreview))

	api.GET("/pair/state", h.getPairingState)
	api.POST("/pair/trust", h.startPairing)

	device := api.Group("/devices/:udid", udidGuard)
	device.GET("/backups", h.listBackups)
	device.POST("/backup", h.startBackup)
	device.POST("/verify", h.startVerify)
	device.DELETE("/backups", h.deleteBackups)
	device.GET("/backups/reclaimable", h.snapshotsReclaimable)
	device.POST("/restore", h.startRestore)
	device.DELETE("/pairing", h.unpairDevice)
	device.POST("/backup-password", h.changeBackupPassword)
	device.PUT("/auto-backup", h.setAutoBackup)
	device.POST("/power", h.controlPower)
	device.POST("/erase", h.eraseDevice)
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
	device.GET("/apps/:bundle/files/download", serveFile(h.openAppFile, serveDownload))
	device.GET("/apps/:bundle/files/preview", serveFile(h.openAppFile, streamPreview))
	device.GET("/media", h.listMedia)
	device.GET("/media/download", serveFile(h.openMedia, serveDownload))
	device.GET("/media/preview", serveFile(h.openMedia, streamPreview))
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
