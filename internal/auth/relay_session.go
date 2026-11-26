package auth

import (
	"context"
	"strconv"
)

// RelaySession identifies the login behind an authenticated relay ticket. A
// JWT ticket follows its refresh family through access-token rotation.
type RelaySession struct {
	AppUser     bool
	OpaqueToken string
	UserID      int64
	FamilyID    int64
	Issuer      string
}

func RelaySessionForToken(accounts AccountStore, token string) (RelaySession, bool) {
	if accounts, ok := accounts.(*JWTAccounts); ok && IsJWT(token) {
		claims, err := accounts.tokens.VerifyAccess(token)
		if err != nil {
			return RelaySession{}, false
		}
		familyID, _ := strconv.ParseInt(claims.Aud, 10, 64)
		return RelaySession{UserID: claims.Sub, FamilyID: familyID, Issuer: claims.Iss}, true
	}
	return RelaySession{OpaqueToken: token}, token != ""
}

func (s RelaySession) Key() SessionKey {
	if s.AppUser {
		return RelayAppSessionKey(s.UserID)
	}
	if s.OpaqueToken != "" {
		return OpaqueSessionKey(s.OpaqueToken)
	}
	return JWTSessionKey(s.UserID, s.FamilyID)
}

func RelayAppSessionKey(userID int64) SessionKey {
	return SessionKey("relay-app-user:" + strconv.FormatInt(userID, 10))
}

func (s RelaySession) Lookup(ctx context.Context, accounts AccountStore) (User, bool, error) {
	if s.AppUser {
		return accounts.Find(ctx, s.UserID)
	}
	if s.OpaqueToken != "" {
		return accounts.Lookup(ctx, s.OpaqueToken)
	}
	if accounts, ok := accounts.(*JWTAccounts); ok {
		return accounts.JWTUser(ctx, s.UserID, s.FamilyID, s.Issuer)
	}
	return User{}, false, nil
}

func (s RelaySession) Watch(ctx context.Context, accounts AccountStore) (context.Context, func()) {
	if accountsWithJWT, ok := accounts.(*JWTAccounts); ok {
		accounts = accountsWithJWT.JWTBackend
	}
	if watcher, ok := accounts.(interface {
		WatchSessionKey(context.Context, SessionKey) (context.Context, func())
	}); ok {
		return watcher.WatchSessionKey(ctx, s.Key())
	}
	return ctx, func() {}
}
