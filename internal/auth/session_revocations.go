package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"sync"
)

var (
	ErrSessionRevoked          = errors.New("session revoked")
	ErrSessionWatchUnavailable = errors.New("session monitoring unavailable")
)

type SessionKey string

func OpaqueSessionKey(token string) SessionKey {
	return SessionKey(fmt.Sprintf("opaque:%x", sha256.Sum256([]byte(token))))
}

func JWTSessionKey(userID, familyID int64) SessionKey {
	return SessionKey("jwt:" + strconv.FormatInt(userID, 10) + ":" + strconv.FormatInt(familyID, 10))
}

// WatchSession registers a connection before it checks whether its token is
// still valid. Once registered, a concurrent signout cancels the connection.
func WatchSession(ctx context.Context, accounts AccountStore, token string) (context.Context, func()) {
	key := OpaqueSessionKey(token)
	if jwtAccounts, ok := accounts.(*JWTAccounts); ok {
		accounts = jwtAccounts.JWTBackend
		if IsJWT(token) {
			claims, err := jwtAccounts.tokens.VerifyAccess(token)
			if err != nil {
				return ctx, func() {}
			}
			familyID, _ := strconv.ParseInt(claims.Aud, 10, 64)
			key = JWTSessionKey(claims.Sub, familyID)
		}
	}
	if watcher, ok := accounts.(interface {
		WatchSessionKey(context.Context, SessionKey) (context.Context, func())
	}); ok {
		return watcher.WatchSessionKey(ctx, key)
	}
	return ctx, func() {}
}

// SessionRevocations tracks the live connections in one store instance.
type SessionRevocations struct {
	mu      sync.Mutex
	watches map[SessionKey]map[context.Context]context.CancelCauseFunc
}

func (s *SessionRevocations) Watch(ctx context.Context, key SessionKey) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	s.mu.Lock()
	if s.watches == nil {
		s.watches = make(map[SessionKey]map[context.Context]context.CancelCauseFunc)
	}
	if s.watches[key] == nil {
		s.watches[key] = make(map[context.Context]context.CancelCauseFunc)
	}
	s.watches[key][ctx] = cancel
	s.mu.Unlock()
	return ctx, func() {
		s.mu.Lock()
		delete(s.watches[key], ctx)
		if len(s.watches[key]) == 0 {
			delete(s.watches, key)
		}
		s.mu.Unlock()
		cancel(nil)
	}
}

func (s *SessionRevocations) Revoke(key SessionKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cancel := range s.watches[key] {
		cancel(ErrSessionRevoked)
	}
	delete(s.watches, key)
}

// CloseAll ends the connections that could miss notifications during an
// interruption of the database listener.
func (s *SessionRevocations) CloseAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, watches := range s.watches {
		for _, cancel := range watches {
			cancel(ErrSessionWatchUnavailable)
		}
	}
	clear(s.watches)
}

func (s *Store) WatchSessionKey(ctx context.Context, key SessionKey) (context.Context, func()) {
	return s.sessionRevocations.Watch(ctx, key)
}
