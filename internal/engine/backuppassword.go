package engine

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/wizier/airvault/internal/ios"
	"github.com/wizier/airvault/internal/ios/backup2"
)

// passwordPromptTimeout is the person's time to enter the passcode on the
// phone; iOS never expires the prompt.
const passwordPromptTimeout = 3 * time.Minute

// ChangeBackupPassword sets the password when oldPassword is empty and
// removes it when newPassword is empty. The encryption flag is reported
// whenever it is known, even on failure: iOS may apply the change without
// answering.
func (e *Engine) ChangeBackupPassword(ctx context.Context, device DeviceID, oldPassword, newPassword string) (BackupPasswordResult, error) {
	udid := string(device)
	baseline := e.backupEncryption(ctx, udid)
	conn, err := e.openBackup2(ctx, udid)
	if err != nil {
		return baseline, err
	}
	err = ctx.Err() // the last point where cancelling changes nothing
	if err == nil {
		err = conn.ChangePassword(ctx, udid, oldPassword, newPassword)
	}
	if err != nil {
		_ = conn.Close()
		return baseline, failure(ctx, "backup password", err)
	}
	expected := expectedEncryption(baseline, oldPassword, newPassword)
	err = e.awaitPasswordChange(ctx, conn, udid, expected)
	_ = conn.Close()
	final := e.backupEncryption(context.WithoutCancel(ctx), udid)
	if !final.EncryptionKnown {
		final = baseline
	}
	switch {
	case err == nil:
		return committedEncryption(final, oldPassword, newPassword), nil
	case isOutcomeUnknown(err) && expected.EncryptionKnown && final == expected:
		return final, nil
	}
	return final, err
}

// awaitPasswordChange takes the device's verdict or, failing that, the
// expected flip of the encryption flag as the change committing.
func (e *Engine) awaitPasswordChange(ctx context.Context, conn *backup2.Conn, udid string, expected BackupPasswordResult) error {
	ctx, cancel := context.WithTimeout(ctx, passwordPromptTimeout)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()
	done := make(chan error, 2)
	wg.Go(func() { done <- passwordVerdict(ctx, conn) })
	if expected.EncryptionKnown {
		wg.Go(func() {
			for sleep(ctx, pollInterval) {
				if e.backupEncryption(ctx, udid) == expected {
					done <- nil
					return
				}
			}
		})
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return outcomeUnknown("no verdict on the password change: " + ctx.Err().Error())
	}
}

func passwordVerdict(ctx context.Context, conn *backup2.Conn) error {
	outcome, err := conn.Outcome(ctx)
	if err == nil {
		err = backup2.Verdict(outcome)
	}
	if err == nil || errors.As(err, new(*backup2.Error)) {
		return verdictFailure(err, false)
	}
	return outcomeUnknown("the password change has no readable verdict: " + err.Error())
}

func outcomeUnknown(detail string) error {
	return &Error{Kind: ErrorOutcomeUnknown, Detail: detail}
}

func isOutcomeUnknown(err error) bool {
	var classified *Error
	return errors.As(err, &classified) && classified.Kind == ErrorOutcomeUnknown
}

func (e *Engine) backupEncryption(ctx context.Context, udid string) BackupPasswordResult {
	encrypted, err := call(ctx, probeTimeout, "backup encryption", func(ctx context.Context) (bool, error) {
		session, err := e.openSession(ctx, udid)
		if err != nil {
			return false, err
		}
		defer session.Close()
		return ios.Value[bool](ctx, session.Lockdown, "com.apple.mobile.backup", "WillEncrypt")
	})
	return BackupPasswordResult{EncryptionKnown: err == nil, Encrypted: encrypted}
}

// expectedEncryption is the flag a committed enable or disable leaves from
// a known opposite state. A flip proves the state changed, not that this
// request changed it; it is the best evidence there is.
func expectedEncryption(baseline BackupPasswordResult, oldPassword, newPassword string) BackupPasswordResult {
	switch {
	case !baseline.EncryptionKnown:
	case oldPassword == "" && newPassword != "" && !baseline.Encrypted:
		return BackupPasswordResult{EncryptionKnown: true, Encrypted: true}
	case oldPassword != "" && newPassword == "" && baseline.Encrypted:
		return BackupPasswordResult{EncryptionKnown: true}
	}
	return BackupPasswordResult{}
}

func committedEncryption(final BackupPasswordResult, oldPassword, newPassword string) BackupPasswordResult {
	switch {
	case newPassword != "" && (oldPassword == "" || !final.EncryptionKnown):
		return BackupPasswordResult{EncryptionKnown: true, Encrypted: true}
	case oldPassword != "" && newPassword == "":
		return BackupPasswordResult{EncryptionKnown: true}
	}
	return final
}
