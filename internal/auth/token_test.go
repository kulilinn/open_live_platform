package auth

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestJWTExchange(t *testing.T) {
	const userID = 78591199061446656
	const issuer = "viewer@example.org"
	s := NewJWTService("example-secret")
	now := time.Unix(1760000000, 0)
	s.now = func() time.Time { return now }

	refresh, refreshClaims, err := s.IssueRefresh(userID, issuer)
	if err != nil {
		t.Fatal(err)
	}
	if refreshClaims.Sub != userID || refreshClaims.Iss != issuer ||
		refreshClaims.Typ != RefreshType || refreshClaims.Aud != strconv.FormatInt(refreshClaims.JTI, 10) ||
		refreshClaims.Iat != now.Unix() || refreshClaims.Exp != now.Add(24*time.Hour).Unix() {
		t.Fatalf("unexpected refresh claims: %+v", refreshClaims)
	}
	if _, err := s.VerifyAccess(refresh); !errors.Is(err, ErrTokenType) {
		t.Fatalf("refresh accepted as access: %v", err)
	}

	access, accessClaims, err := s.ExchangeRefresh(refresh)
	if err != nil {
		t.Fatal(err)
	}
	if accessClaims.Sub != userID || accessClaims.Iss != issuer ||
		accessClaims.Typ != AccessType || accessClaims.Aud != refreshClaims.Aud ||
		accessClaims.JTI == refreshClaims.JTI || accessClaims.Exp != now.Add(30*time.Minute).Unix() {
		t.Fatalf("unexpected access claims: %+v", accessClaims)
	}
	if got, err := s.VerifyAccess(access); err != nil || got != accessClaims {
		t.Fatalf("access verification = (%+v, %v)", got, err)
	}
	if _, _, err := s.ExchangeRefresh(access); !errors.Is(err, ErrTokenType) {
		t.Fatalf("access accepted for exchange: %v", err)
	}
	segments := strings.Split(access, ".")
	segments[2] = strings.Repeat("A", len(segments[2]))
	if _, err := s.VerifyAccess(strings.Join(segments, ".")); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("tampered token verified: %v", err)
	}

	now = now.Add(30 * time.Minute)
	if _, err := s.VerifyAccess(access); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("access at expiry = %v", err)
	}
	if _, _, err := s.ExchangeRefresh(refresh); err != nil {
		t.Fatalf("refresh after access expiry: %v", err)
	}
	now = now.Add(23*time.Hour + 30*time.Minute)
	if _, _, err := s.ExchangeRefresh(refresh); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("refresh at expiry = %v", err)
	}
}

func TestJWTRustSignature(t *testing.T) {
	// Built independently with the original decimal subject and hex MD5
	// secret derivation. The second fixture has a valid HMAC for another alg.
	const payload = "eyJzdWIiOjc4NTkxMTk5MDYxNDQ2NjU2LCJpYXQiOjE3NjAwMDAwMDAsImV4cCI6MTc2MDA4NjQwMCwidHlwIjoiUkVGUkVTSCIsImF1ZCI6IjQyNDIiLCJqdGkiOjQyNDIsImlzcyI6InZpZXdlckBleGFtcGxlLm9yZyJ9"
	const valid = jwtHeader + "." + payload + ".7DzOQYosEY_HxuOqD9L6cdpWqCLfMu3cx3kme9pKSEg"
	const otherAlgorithm = "eyJ0eXAiOiJKV1QiLCJhbGciOiJub25lIn0." + payload + ".t5npnWQXBbYy9_2gkFcsqCLuqg3YuzdESMaRe928pOo"
	s := NewJWTService("example-secret")
	s.now = func() time.Time { return time.Unix(1760000001, 0) }
	claims, err := s.VerifyRefresh(valid)
	if err != nil || claims.Sub != 78591199061446656 || claims.JTI != 4242 ||
		claims.Aud != "4242" || claims.Iss != "viewer@example.org" {
		t.Fatalf("Rust signature verification = (%+v, %v)", claims, err)
	}
	if _, err := s.VerifyRefresh(otherAlgorithm); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("wrong algorithm verified: %v", err)
	}
	if _, err := NewJWTService("").VerifyRefresh(valid); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("missing secret accepted: %v", err)
	}
}
