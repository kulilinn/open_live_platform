package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"time"
)

var (
	ErrExchangeUnavailable = errors.New("login code exchange is unavailable")
	ErrInvalidLoginCode    = errors.New("invalid or expired login code")
)

const loginCodeTTL = 5 * time.Minute

type LoginGrant struct {
	UserID    int64
	FamilyID  int64
	Issuer    string
	ExpiresAt time.Time
}

type loginGrantStore interface {
	SaveLoginGrant(context.Context, [32]byte, LoginGrant) error
	TakeLoginGrant(context.Context, [32]byte) (LoginGrant, bool, error)
	ReplaceJWTSession(ctx context.Context, grant LoginGrant, user User, newFamilyID int64) (bool, error)
}

// NewLoginCode binds a random, short-lived transfer code to an active access token's family.
func NewLoginCode(ctx context.Context, accounts AccountStore, token string) (string, error) {
	jwtAccounts, ok := accounts.(*JWTAccounts)
	if !ok {
		return "", ErrExchangeUnavailable
	}
	store, ok := jwtAccounts.JWTBackend.(loginGrantStore)
	if !ok {
		return "", ErrExchangeUnavailable
	}
	claims, err := verifyAccessIdentity(ctx, accounts, token)
	if err != nil {
		return "", err
	}
	familyID, _ := strconv.ParseInt(claims.Aud, 10, 64)
	var random [16]byte
	rand.Read(random[:])
	code := hex.EncodeToString(random[:])
	grant := LoginGrant{UserID: claims.Sub, FamilyID: familyID, Issuer: claims.Iss, ExpiresAt: time.Now().Add(loginCodeTTL)}
	if err := store.SaveLoginGrant(ctx, sha256.Sum256([]byte(code)), grant); err != nil {
		return "", err
	}
	return code, nil
}

func ExchangeLoginCode(ctx context.Context, accounts AccountStore, code string) (Session, error) {
	jwtAccounts, ok := accounts.(*JWTAccounts)
	if !ok {
		return Session{}, ErrExchangeUnavailable
	}
	store, ok := jwtAccounts.JWTBackend.(loginGrantStore)
	if !ok {
		return Session{}, ErrExchangeUnavailable
	}
	if len(code) != 32 {
		return Session{}, ErrInvalidLoginCode
	}
	grant, found, err := store.TakeLoginGrant(ctx, sha256.Sum256([]byte(code)))
	if err != nil {
		return Session{}, err
	}
	if !found {
		return Session{}, ErrInvalidLoginCode
	}
	user, found, err := jwtAccounts.JWTUser(ctx, grant.UserID, grant.FamilyID, grant.Issuer)
	if err != nil {
		return Session{}, err
	}
	if !found {
		return Session{}, ErrInvalidLoginCode
	}
	if user.Moderation.Banned() {
		return Session{}, BannedError{Moderation: user.Moderation}
	}
	token, claims, err := jwtAccounts.tokens.IssueRefresh(user.ID, user.Email)
	if err != nil {
		return Session{}, err
	}
	replaced, err := store.ReplaceJWTSession(ctx, grant, user, claims.JTI)
	if err != nil {
		return Session{}, err
	}
	if !replaced {
		return Session{}, ErrInvalidLoginCode
	}
	return Session{Token: token, User: user}, nil
}

func (s *Store) SaveLoginGrant(ctx context.Context, digest [32]byte, grant LoginGrant) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loginGrants == nil {
		s.loginGrants = make(map[[32]byte]LoginGrant)
	}
	now := time.Now()
	if !now.Before(s.nextLoginGrantSweep) {
		for digest, grant := range s.loginGrants {
			if !now.Before(grant.ExpiresAt) {
				delete(s.loginGrants, digest)
			}
		}
		s.nextLoginGrantSweep = now.Add(time.Minute)
	}
	s.loginGrants[digest] = grant
	return nil
}

func (s *Store) TakeLoginGrant(ctx context.Context, digest [32]byte) (LoginGrant, bool, error) {
	if err := ctx.Err(); err != nil {
		return LoginGrant{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	grant, found := s.loginGrants[digest]
	delete(s.loginGrants, digest)
	return grant, found && time.Now().Before(grant.ExpiresAt), nil
}

func (s *Store) ReplaceJWTSession(ctx context.Context, grant LoginGrant, user User, newFamilyID int64) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	login, ok := s.users[user.accountKey()]
	if !ok || login.user.ID != grant.UserID || login.user.Moderation.Banned() || !time.Now().Before(grant.ExpiresAt) || s.jwtSessions[grant.UserID] != grant.FamilyID {
		return false, nil
	}
	s.jwtSessions[grant.UserID] = newFamilyID
	s.sessionRevocations.Revoke(JWTSessionKey(grant.UserID, grant.FamilyID))
	return true, nil
}
