package postgres

import (
	"context"
	"strings"

	"open_live_platform/internal/auth"
)

const sessionRevocationPrefix = "session-revoked:"

func (s *Store) WatchSessionKey(ctx context.Context, key auth.SessionKey) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	ctx, stop := s.sessionRevocations.Watch(ctx, key)
	// Register first so an interruption starting after the readiness check
	// also finds and cancels this connection.
	if !s.eventReady.Load() {
		cancel(auth.ErrSessionWatchUnavailable)
	}
	return ctx, func() {
		stop()
		cancel(nil)
	}
}

func (s *Store) readSessionRevocation(payload string) bool {
	key, ok := strings.CutPrefix(payload, sessionRevocationPrefix)
	if ok {
		s.sessionRevocations.Revoke(auth.SessionKey(key))
	}
	return ok
}
