// Command airvault is the AirVault daemon: it discovers paired iPhones, backs
// them up over Wi-Fi to the NAS, and serves the web UI + JSON API.
package main

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	airvault "github.com/wizier/airvault"
	"github.com/wizier/airvault/internal/auth"
	"github.com/wizier/airvault/internal/config"
	"github.com/wizier/airvault/internal/engine"
	"github.com/wizier/airvault/internal/events"
	"github.com/wizier/airvault/internal/handler"
	airlog "github.com/wizier/airvault/internal/logging"
	"github.com/wizier/airvault/internal/objectstore"
	"github.com/wizier/airvault/internal/service"
	"github.com/wizier/airvault/internal/storage"
	"github.com/wizier/airvault/internal/version"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
	"github.com/pressly/goose/v3"

	_ "modernc.org/sqlite"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	// Installed first, so a signal during a long startup still unwinds through
	// the deferred cleanup instead of killing the process.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg := config.Load()

	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return fmt.Errorf("AIRVAULT_LOG_LEVEL must be debug, info, warn, or error: %w", err)
	}
	airlog.Setup(level)
	if !engine.InitLogging(cfg.LogLevel) {
		slog.Warn("engine: Rust tracing bridge could not be installed")
	}
	// Paths are logged every start: the container resolves them, so support
	// questions start from the log rather than from guessing the layout.
	slog.Info("AirVault starting", "version", version.AppVersion, "listen", cfg.ListenAddr,
		"log_level", cfg.LogLevel, "config", cfg.ConfigDir,
		"backups", cfg.BackupDir, "lockdown", cfg.LockdownDir)

	for _, d := range []string{cfg.ConfigDir, cfg.BackupDir, cfg.LockdownDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("creating directory %s: %w", d, err)
		}
	}
	credentials, showToken, err := auth.Load(cfg.ConfigDir, cfg.AuthToken)
	if err != nil {
		return fmt.Errorf("loading HTTP credentials: %w", err)
	}
	if showToken != "" {
		slog.Warn("HTTP authentication token (set AIRVAULT_AUTH_TOKEN to override)",
			"username", auth.Username, "token", showToken, "path", cfg.ConfigDir+"/auth-token")
	}
	// Pragmas in DSN apply per-connection (foreign_keys requires this).
	// _txlock=immediate: deferred tx upgrading read→write under WAL fail with
	// BUSY_SNAPSHOT, which busy_timeout does not retry.
	separator := "?"
	if strings.Contains(cfg.DatabaseURL, "?") {
		separator = "&"
	}
	const sqlitePragmas = "_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)" +
		"&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	dsn := cfg.DatabaseURL + separator + sqlitePragmas
	db, err := sqlx.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	// Cleared by a shutdown that leaves work running, which the closes would race.
	closeStorage := true
	defer func() {
		if closeStorage {
			_ = db.Close()
		}
	}()
	// A handful of connections is plenty for a single-file SQLite DB.
	const dbMaxConns = 4
	db.SetMaxOpenConns(dbMaxConns)
	db.SetMaxIdleConns(dbMaxConns)

	goose.SetBaseFS(airvault.MigrationsFS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("setting goose dialect: %w", err)
	}
	if err := goose.Up(db.DB, "migrations"); err != nil {
		return fmt.Errorf("running migrations: %w", err)
	}

	store := storage.NewStore(db)

	staticFS, err := fs.Sub(airvault.WebFS, "web/dist")
	if err != nil {
		return fmt.Errorf("sub web FS: %w", err)
	}

	objects, err := objectstore.New(cfg.BackupDir)
	if err != nil {
		return fmt.Errorf("opening backup object store: %w", err)
	}
	defer func() {
		if closeStorage {
			if err := objects.Close(); err != nil {
				slog.Warn("close backup object store", "error", err)
			}
		}
	}()
	// Upload staging lives on a mounted volume (multi-GiB .ipa files must not
	// land in the container's writable layer); leftovers are cleared on start.
	uploads := filepath.Join(cfg.ConfigDir, "uploads")
	if err := os.RemoveAll(uploads); err != nil {
		return fmt.Errorf("clearing upload staging: %w", err)
	}
	if err := os.MkdirAll(uploads, 0o755); err != nil {
		return fmt.Errorf("creating upload staging: %w", err)
	}

	eng, err := engine.New(engine.Config{
		BackupRoot:  cfg.BackupDir,
		PairingRoot: cfg.LockdownDir,
		MuxAddress:  cfg.MuxAddress,
	})
	if err != nil {
		return fmt.Errorf("creating device engine: %w", err)
	}
	defer func() {
		if closeStorage {
			_ = eng.Close()
		}
	}()
	bus := events.New()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	svc := service.New(ctx, store, eng, objects, uploads, bus)
	backupSources, err := svc.ReconcileBackupStore(ctx)
	if err != nil {
		return fmt.Errorf("reconciling backup storage: %w", err)
	}
	// Background so the HTTP listener comes up immediately; a backup admitted
	// before presence lands waits for reachability inside the run.
	svc.StartWatch(ctx)
	svc.StartMaintenance(ctx, backupSources)
	var httpShutdownIncomplete atomic.Bool
	// Abort in-flight work and join supervised workers before the deferred
	// closes. The bound is a last-resort process-exit escape, not a normal
	// cancellation path.
	defer func() {
		cancel()
		workersStopped := svc.Wait(10 * time.Second)
		if !workersStopped {
			slog.Warn("service workers did not stop before shutdown deadline")
		}
		if !workersStopped || httpShutdownIncomplete.Load() {
			// Skip the explicit closes: they would race work still running. Process
			// teardown closes descriptors; startup reconciliation resolves staging.
			closeStorage = false
		}
	}()

	h := handler.New(credentials, svc, bus, staticFS)
	e := h.Router()

	sc := echo.StartConfig{
		Address:         cfg.ListenAddr,
		HideBanner:      true,
		HidePort:        true,
		GracefulTimeout: 10 * time.Second,
		OnShutdownError: func(err error) {
			httpShutdownIncomplete.Store(true)
			slog.Warn("HTTP requests did not stop before shutdown deadline", "error", err)
		},
		BeforeServeFunc: func(s *http.Server) error {
			configureHTTPServer(ctx, s)
			return nil
		},
	}
	slog.Info("server starting", "addr", sc.Address)
	// Start shuts the server down gracefully once a signal cancels ctx. Transfers
	// share ctx, so they cancel in parallel with HTTP shutdown: waiting out its
	// deadline risks Docker killing jobs before they persist terminal state.
	if err := sc.Start(ctx, e); err != nil {
		return fmt.Errorf("server error: %w", err)
	}
	slog.Info("shutting down...")
	return nil
}

// configureHTTPServer makes base every request's parent, so cancelling it
// unblocks in-flight requests and shutdown doesn't burn GracefulTimeout.
func configureHTTPServer(base context.Context, server *http.Server) {
	// Clear Echo's 30s whole-request deadlines: IPA uploads, SSE and device
	// operations legitimately take minutes. Body limits and native operation
	// deadlines bound them instead.
	server.ReadTimeout = 0
	server.WriteTimeout = 0
	server.ReadHeaderTimeout = 10 * time.Second
	server.IdleTimeout = 60 * time.Second
	server.BaseContext = func(net.Listener) context.Context { return base }
}
