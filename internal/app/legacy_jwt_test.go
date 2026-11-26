package app_test

import (
	"crypto/md5"
	"encoding/base64"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/store/postgres"
)

func TestPostgresImportLegacyRedisSession(t *testing.T) {
	config, db := postgresTestDatabase(t)
	ctx := t.Context()
	config.JWTSecret = "legacy-test-secret"
	store, err := postgres.Open(ctx, config.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Shutdown()
	platform, err := app.New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()

	value, err := base64.StdEncoding.DecodeString("AZCHdkc2FwETAAAAAAAAAEV4YW1wbGVARXhhbXBsZS5vcmexaN46AAAAAAOQh3ZHNhcBAbBo3joAAAAAAR4AAAAAAAAAMjAyNS0xMC0wMVQwMTowMjowMy4xMjM0NTY3ODlaAg==")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	legacy, err := auth.ParseLegacyJWTSession(auth.LegacySessionRecord{
		Key: "{78591199061446657}|auth_user|LiveMedia", Value: value, ExpiresAt: now.Add(10 * time.Minute),
	}, "LiveMedia")
	if err != nil {
		t.Fatal(err)
	}
	importSession := func(session auth.LegacyJWTSession, want postgres.LegacyJWTImportResult) {
		t.Helper()
		if got, err := store.ImportLegacyJWTSession(ctx, session); err != nil || got != want {
			t.Fatalf("import = %s, error = %v, want %s", got, err, want)
		}
	}
	issue := func(typ, issuer string, familyID int64) string {
		t.Helper()
		id := familyID
		if typ == auth.AccessType {
			id++
		}
		key := []byte(fmt.Sprintf("%x", md5.Sum([]byte(fmt.Sprintf("%s:%d", config.JWTSecret, legacy.UserID)))))
		token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"sub": legacy.UserID, "iss": issuer, "aud": strconv.FormatInt(familyID, 10), "typ": typ,
			"jti": id, "iat": now.Add(-time.Minute).Unix(), "exp": now.Add(time.Hour).Unix(),
		}).SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	lookup := func(token string, accepted bool) {
		t.Helper()
		user, ok, err := platform.Accounts.Lookup(ctx, token)
		if err != nil || ok != accepted || (ok && user.ID != legacy.UserID) {
			t.Fatalf("lookup = %t, user ID = %d, error = %v, want accepted = %t", ok, user.ID, err, accepted)
		}
	}

	importSession(legacy, postgres.LegacyJWTAccountMismatch)
	_, err = db.Exec(ctx, `INSERT INTO users (id,email,account,password_salt,password_hash) VALUES ($1,$2,'Legacy User','','')`,
		legacy.UserID, auth.NormalizeEmail(legacy.Email))
	if err != nil {
		t.Fatal(err)
	}
	mismatch := legacy
	mismatch.Email = "other@example.org"
	importSession(mismatch, postgres.LegacyJWTAccountMismatch)
	expired := legacy
	expired.ExpiresAt = now.Add(-time.Minute)
	importSession(expired, postgres.LegacyJWTExpired)

	access := issue(auth.AccessType, legacy.Email, legacy.FamilyID)
	refresh := issue(auth.RefreshType, legacy.Email, legacy.FamilyID)
	lookup(access, false)
	importSession(legacy, postgres.LegacyJWTImported)
	importSession(legacy, postgres.LegacyJWTExistingSession)
	lookup(access, true)
	lookup(issue(auth.AccessType, auth.NormalizeEmail(legacy.Email), legacy.FamilyID), false)
	lookup(issue(auth.AccessType, legacy.Email, legacy.FamilyID+5), false)
	exchanged, ok, err := platform.Accounts.Refresh(ctx, refresh)
	if err != nil || !ok {
		t.Fatalf("legacy refresh accepted = %t, error = %v", ok, err)
	}
	lookup(exchanged.Token, true)

	if _, err := db.Exec(ctx, `UPDATE jwt_sessions SET expires_at=now() WHERE user_id=$1`, legacy.UserID); err != nil {
		t.Fatal(err)
	}
	lookup(access, false)
	if _, ok, err := platform.Accounts.Refresh(ctx, refresh); err != nil || ok {
		t.Fatalf("legacy refresh after Redis expiry accepted = %t, error = %v", ok, err)
	}
	if _, err := db.Exec(ctx, `UPDATE jwt_sessions SET expires_at=$2 WHERE user_id=$1`, legacy.UserID, legacy.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	if err := platform.Accounts.Revoke(ctx, access); err != nil {
		t.Fatal(err)
	}
	importSession(legacy, postgres.LegacyJWTExistingSession)
	lookup(access, false)
	if _, ok, err := platform.Accounts.Refresh(ctx, refresh); err != nil || ok {
		t.Fatalf("legacy refresh after signout accepted = %t, error = %v", ok, err)
	}
	if err := store.SaveJWTSession(ctx, legacy.UserID, legacy.FamilyID+20); err != nil {
		t.Fatal(err)
	}
	lookup(issue(auth.AccessType, auth.NormalizeEmail(legacy.Email), legacy.FamilyID+20), true)
	importSession(legacy, postgres.LegacyJWTExistingSession)
	lookup(access, false)
}
