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

type GrantPurpose int

const (
	OAuthCode   GrantPurpose = 1
	ServiceCode GrantPurpose = 2
)

type TrustedGrant struct {
	Purpose            GrantPurpose
	ClientID, Redirect string
	UserID, FamilyID   int64
	Issuer             string
	ExpiresAt          time.Time
}

type GrantBinding struct {
	Purpose            GrantPurpose
	ClientID, Redirect string
	UserID             int64
}

type TrustedGrantStore interface {
	SaveTrustedGrant(context.Context, [32]byte, TrustedGrant) (bool, error)
	ConsumeTrustedGrant(context.Context, [32]byte, GrantBinding) (int64, error)
}

var ErrTrustedGrant = errors.New("authorization code is invalid or expired")

func verifyAccessIdentity(ctx context.Context, accounts AccountStore, token string) (TokenClaims, error) {
	service, ok := accounts.(*JWTAccounts)
	if !ok {
		return TokenClaims{}, ErrExchangeUnavailable
	}
	claims, err := service.tokens.VerifyAccess(token)
	if err != nil {
		return TokenClaims{}, ErrInvalidCredentials
	}
	family, _ := strconv.ParseInt(claims.Aud, 10, 64)
	user, found, err := service.JWTUser(ctx, claims.Sub, family, claims.Iss)
	if err != nil {
		return TokenClaims{}, err
	}
	if !found {
		return TokenClaims{}, ErrInvalidCredentials
	}
	if user.Moderation.Banned() {
		return TokenClaims{}, BannedError{Moderation: user.Moderation}
	}
	return claims, nil
}

func IssueTrustedGrant(ctx context.Context, accounts AccountStore, storage TrustedGrantStore, token string, binding GrantBinding) (string, error) {
	if storage == nil {
		return "", ErrExchangeUnavailable
	}
	claims, err := verifyAccessIdentity(ctx, accounts, token)
	if err != nil {
		return "", err
	}
	if binding.UserID != claims.Sub {
		return "", ErrInvalidCredentials
	}
	expiry := time.Now().Add(loginCodeTTL)
	if accessExpiry := time.Unix(claims.Exp, 0); accessExpiry.Before(expiry) {
		expiry = accessExpiry
	}
	family, _ := strconv.ParseInt(claims.Aud, 10, 64)
	grant := TrustedGrant{Purpose: binding.Purpose, ClientID: binding.ClientID, Redirect: binding.Redirect,
		UserID: claims.Sub, FamilyID: family, Issuer: claims.Iss, ExpiresAt: expiry}
	var random [16]byte
	rand.Read(random[:])
	code := hex.EncodeToString(random[:])
	saved, err := storage.SaveTrustedGrant(ctx, sha256.Sum256([]byte(code)), grant)
	if err != nil {
		return "", err
	}
	if !saved {
		return "", ErrInvalidCredentials
	}
	return code, nil
}

func VerifyTrustedGrant(ctx context.Context, accounts AccountStore, storage TrustedGrantStore, code string, binding GrantBinding) (User, error) {
	if storage == nil {
		return User{}, ErrExchangeUnavailable
	}
	if len(code) != 32 {
		return User{}, ErrTrustedGrant
	}
	if _, err := hex.DecodeString(code); err != nil {
		return User{}, ErrTrustedGrant
	}
	id, err := storage.ConsumeTrustedGrant(ctx, sha256.Sum256([]byte(code)), binding)
	if err != nil {
		return User{}, err
	}
	user, found, err := accounts.Find(ctx, id)
	if err != nil {
		return User{}, err
	}
	if !found || user.Moderation.Banned() {
		return User{}, ErrTrustedGrant
	}
	return user, nil
}

func (s *Store) SaveTrustedGrant(ctx context.Context, digest [32]byte, grant TrustedGrant) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	now := time.Now()
	if !s.trustedGrantAvailable(grant, now) {
		return false, nil
	}
	if s.trustedGrants == nil {
		s.trustedGrants = make(map[[32]byte]TrustedGrant)
	}
	if !now.Before(s.nextTrustedSweep) {
		for key, row := range s.trustedGrants {
			if !now.Before(row.ExpiresAt) {
				delete(s.trustedGrants, key)
			}
		}
		s.nextTrustedSweep = now.Add(time.Minute)
	}
	if _, exists := s.trustedGrants[digest]; exists {
		return false, nil
	}
	s.trustedGrants[digest] = grant
	return true, nil
}

func (s *Store) ConsumeTrustedGrant(ctx context.Context, digest [32]byte, match GrantBinding) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	grant, found := s.trustedGrants[digest]
	if !found || grant.Purpose != match.Purpose || grant.ClientID != match.ClientID || grant.Redirect != match.Redirect ||
		(match.UserID != 0 && match.UserID != grant.UserID) || !s.trustedGrantAvailable(grant, time.Now()) {
		return 0, ErrTrustedGrant
	}
	delete(s.trustedGrants, digest)
	return grant.UserID, nil
}

func (s *Store) trustedGrantAvailable(grant TrustedGrant, now time.Time) bool {
	if !now.Before(grant.ExpiresAt) || s.jwtSessions[grant.UserID] != grant.FamilyID {
		return false
	}
	login, found := s.users[(User{ID: grant.UserID, Email: grant.Issuer}).accountKey()]
	return found && login.user.ID == grant.UserID && !login.user.Moderation.Banned()
}
