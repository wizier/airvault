package engine

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"time"

	"github.com/wizier/airvault/internal/ios"
	"github.com/wizier/airvault/internal/ios/afc"
)

const (
	syncLockFile    = "/com.apple.itunes.lock_sync"
	syncWillStart   = "com.apple.itunes-mobdev.syncWillStart"
	syncLockRequest = "com.apple.itunes-mobdev.syncLockRequest"
	syncDidStart    = "com.apple.itunes-mobdev.syncDidStart"
	syncDidFinish   = "com.apple.itunes-mobdev.syncDidFinish"
	syncFailed      = "com.apple.itunes-mobdev.syncFailedToStart"
	syncLockWait    = 10 * time.Second
	syncLockRetry   = 200 * time.Millisecond
	teardownTimeout = 2 * time.Second
)

// syncSession announces a sync as Finder does and holds the device's sync
// lock for it; a locked phone backs up only inside one.
type syncSession struct {
	proxy  *ios.NotificationProxy
	files  *afc.Client
	lock   *afc.File
	locked bool
}

func (e *Engine) startSync(ctx context.Context, udid string) (*syncSession, error) {
	conn, err := call(ctx, probeTimeout, "sync session", func(ctx context.Context) (net.Conn, error) {
		return e.openService(ctx, udid, ios.NotificationProxyService)
	})
	if err != nil {
		return nil, err
	}
	s := &syncSession{proxy: ios.NewNotificationProxy(conn)}
	if err := s.acquire(ctx, e, udid); err != nil {
		if cleanupErr := s.finish(context.WithoutCancel(ctx)); cleanupErr != nil {
			slog.WarnContext(ctx, "sync session left behind", "udid", udid, "error", cleanupErr)
		}
		return nil, failure(ctx, "sync session", err)
	}
	return s, nil
}

func (s *syncSession) acquire(ctx context.Context, e *Engine, udid string) error {
	step := func(op func(context.Context) error) error {
		ctx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		return op(ctx)
	}
	err := step(func(ctx context.Context) error { return s.proxy.Post(ctx, syncWillStart) })
	if err == nil {
		err = step(func(ctx context.Context) (err error) {
			s.files, err = e.dialAFC(ctx, afcKey{udid: udid, source: AFCMedia})
			return err
		})
	}
	if err == nil {
		err = step(func(ctx context.Context) (err error) {
			s.lock, err = s.files.Open(ctx, syncLockFile, afc.ReadWrite)
			return err
		})
	}
	if err == nil {
		err = step(func(ctx context.Context) error { return s.proxy.Post(ctx, syncLockRequest) })
	}
	if err == nil {
		err = s.takeLock(ctx)
	}
	if err == nil {
		err = step(func(ctx context.Context) error { return s.proxy.Post(ctx, syncDidStart) })
	}
	return err
}

// takeLock waits while another host's sync holds the lock.
func (s *syncSession) takeLock(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, syncLockWait)
	defer cancel()
	for {
		err := s.lock.Lock(ctx, afc.LockExclusive)
		if err == nil {
			s.locked = true
			return nil
		}
		if !errors.Is(err, afc.ErrOpWouldBlock) {
			return err
		}
		if !sleep(ctx, syncLockRetry) {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return &Error{Kind: ErrorBusy, Detail: "another sync holds the device's sync lock"}
			}
			return ctx.Err()
		}
	}
}

// finish releases what acquire took, each step bounded, and reports what
// could not be undone.
func (s *syncSession) finish(ctx context.Context) error {
	step := func(op func(context.Context) error) error {
		ctx, cancel := context.WithTimeout(ctx, teardownTimeout)
		defer cancel()
		return op(ctx)
	}
	var errs []error
	if s.locked {
		errs = append(errs, step(func(ctx context.Context) error { return s.lock.Lock(ctx, afc.LockRelease) }))
	}
	if s.lock != nil {
		errs = append(errs, step(s.lock.Close))
	}
	if s.files != nil {
		_ = s.files.Close()
	}
	// As Finder: a sync that never took the lock never started.
	ending := syncDidFinish
	if !s.locked {
		ending = syncFailed
	}
	errs = append(errs, step(func(ctx context.Context) error { return s.proxy.Post(ctx, ending) }))
	_ = s.proxy.Close()
	return errors.Join(errs...)
}
