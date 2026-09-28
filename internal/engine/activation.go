package engine

import (
	"context"
	"log/slog"

	"github.com/wizier/airvault/internal/ios"
)

// ActivationState falls back to lockdown when the daemon has no answer.
func (e *Engine) ActivationState(ctx context.Context, device DeviceID) (string, error) {
	return call(ctx, deviceWorkTimeout, "activation state", func(ctx context.Context) (string, error) {
		if conn, err := e.openService(ctx, string(device), ios.ActivationService); err == nil {
			if state, err := ios.ActivationState(ctx, conn); err == nil {
				return state, nil
			}
		}
		session, err := e.openSession(ctx, string(device))
		if err != nil {
			return "", err
		}
		defer session.Close()
		return ios.Value[string](ctx, session.Lockdown, "", "ActivationState")
	})
}

func (e *Engine) ActivationSessionInfo(ctx context.Context, device DeviceID) ([]byte, error) {
	return call(ctx, deviceWorkTimeout, "activation session info", func(ctx context.Context) ([]byte, error) {
		conn, err := e.openService(ctx, string(device), ios.ActivationService)
		if err != nil {
			return nil, err
		}
		return ios.ActivationSessionInfo(ctx, conn)
	})
}

func (e *Engine) ActivationInfo(ctx context.Context, device DeviceID, handshake []byte) ([]byte, error) {
	if len(handshake) == 0 {
		return nil, &Error{Kind: ErrorInvalidArgument, Detail: "empty activation handshake"}
	}
	return call(ctx, deviceWorkTimeout, "activation info", func(ctx context.Context) ([]byte, error) {
		conn, err := e.openService(ctx, string(device), ios.ActivationService)
		if err != nil {
			return nil, err
		}
		return ios.ActivationInfo(ctx, conn, handshake)
	})
}

func (e *Engine) ActivationFinish(ctx context.Context, device DeviceID, record []byte, headers map[string]string) error {
	if len(record) == 0 {
		return &Error{Kind: ErrorInvalidArgument, Detail: "empty activation record"}
	}
	return do(context.WithoutCancel(ctx), deviceWorkTimeout, "activation", func(ctx context.Context) error {
		conn, err := e.openService(ctx, string(device), ios.ActivationService)
		if err != nil {
			return err
		}
		if err := ios.Activate(ctx, conn, record, headers); err != nil {
			return err
		}
		// The phone is activated once the daemon accepts; the acknowledgement is best effort.
		if err := e.acknowledgeActivation(ctx, device); err != nil {
			slog.WarnContext(ctx, "activation acknowledgement failed", "udid", device, "error", err)
		}
		return nil
	})
}

func (e *Engine) acknowledgeActivation(ctx context.Context, device DeviceID) error {
	session, err := e.openSession(ctx, string(device))
	if err != nil {
		return err
	}
	defer session.Close()
	return session.SetValue(ctx, "", "ActivationStateAcknowledged", true)
}
