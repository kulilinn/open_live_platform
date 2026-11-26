package auth

import (
	"context"
	"strconv"
	"strings"
)

// JWTBackend supplies credentials and the active refresh family for each user.
type JWTBackend interface {
	AccountStore
	ResolveGoogleUser(ctx context.Context, subject, email, language string, mayLink bool) (id int64, created bool, err error)
	IssueVerifiedSession(context.Context, User) (Session, error)
	VerifyPassword(ctx context.Context, email, password string) (User, error)
	SaveJWTSession(ctx context.Context, userID, familyID int64) error
	JWTUser(ctx context.Context, userID, familyID int64, email string) (User, bool, error)
	RevokeJWTSession(ctx context.Context, userID, familyID int64) error
}

// JWTAccounts adds compatibility-mode JWT sessions to an existing account store.
type JWTAccounts struct {
	JWTBackend
	tokens *JWTService
}

func WithJWT(store JWTBackend, secret string) AccountStore {
	if secret == "" {
		return store
	}
	return &JWTAccounts{JWTBackend: store, tokens: NewJWTService(secret)}
}

func (s *JWTAccounts) SignInJWT(ctx context.Context, email, password string) (Session, error) {
	user, err := s.VerifyPassword(ctx, email, password)
	if err != nil {
		return Session{}, err
	}
	return s.signInUser(ctx, user)
}

func (s *JWTAccounts) signInUser(ctx context.Context, user User) (Session, error) {
	if user.Moderation.Banned() {
		return Session{}, BannedError{Moderation: user.Moderation}
	}
	token, claims, err := s.tokens.IssueRefresh(user.ID, user.Email)
	if err != nil {
		return Session{}, err
	}
	if err := s.SaveJWTSession(ctx, user.ID, claims.JTI); err != nil {
		return Session{}, err
	}
	return Session{Token: token, User: user}, nil
}

func (s *JWTAccounts) Lookup(ctx context.Context, token string) (User, bool, error) {
	if !IsJWT(token) {
		return s.JWTBackend.Lookup(ctx, token)
	}
	claims, err := s.tokens.VerifyAccess(token)
	if err != nil {
		return User{}, false, nil
	}
	return s.sessionUser(ctx, claims)
}

func (s *JWTAccounts) Bearer(ctx context.Context, authorization string) (User, bool, error) {
	token, ok := strings.CutPrefix(authorization, "Bearer ")
	if !ok {
		return User{}, false, nil
	}
	return s.Lookup(ctx, token)
}

func (s *JWTAccounts) Refresh(ctx context.Context, token string) (Session, bool, error) {
	if !IsJWT(token) {
		return s.JWTBackend.Refresh(ctx, token)
	}
	access, claims, err := s.tokens.ExchangeRefresh(token)
	if err != nil {
		return Session{}, false, nil
	}
	user, ok, err := s.sessionUser(ctx, claims)
	if err != nil || !ok {
		return Session{}, ok, err
	}
	if user.Moderation.Banned() {
		return Session{}, false, BannedError{Moderation: user.Moderation}
	}
	return Session{Token: access, User: user}, true, nil
}

func (s *JWTAccounts) Revoke(ctx context.Context, token string) error {
	if !IsJWT(token) {
		return s.JWTBackend.Revoke(ctx, token)
	}
	claims, err := s.tokens.VerifyAccess(token)
	if err != nil {
		return nil
	}
	familyID, _ := strconv.ParseInt(claims.Aud, 10, 64)
	return s.RevokeJWTSession(ctx, claims.Sub, familyID)
}

func (s *JWTAccounts) sessionUser(ctx context.Context, claims TokenClaims) (User, bool, error) {
	familyID, _ := strconv.ParseInt(claims.Aud, 10, 64)
	return s.JWTUser(ctx, claims.Sub, familyID, claims.Iss)
}

// rand.Text session tokens contain no dots, unlike compact JWTs.
func IsJWT(token string) bool {
	return strings.Count(token, ".") == 2
}

func (s *Store) SaveJWTSession(ctx context.Context, userID, familyID int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jwtSessions[userID] = familyID
	return nil
}

func (s *Store) JWTUser(ctx context.Context, userID, familyID int64, email string) (User, bool, error) {
	if err := ctx.Err(); err != nil {
		return User{}, false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.jwtSessions[userID] != familyID {
		return User{}, false, nil
	}
	login, ok := s.users[email]
	if !ok || login.user.ID != userID {
		return User{}, false, nil
	}
	return login.user, true, nil
}

func (s *Store) RevokeJWTSession(ctx context.Context, userID, familyID int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.jwtSessions[userID] == familyID {
		delete(s.jwtSessions, userID)
		s.sessionRevocations.Revoke(JWTSessionKey(userID, familyID))
	}
	return nil
}
