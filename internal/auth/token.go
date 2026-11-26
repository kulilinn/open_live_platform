package auth

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

const (
	RefreshType = "REFRESH"
	AccessType  = "ACCESS"

	refreshTTL = 24 * time.Hour
	accessTTL  = 30 * time.Minute

	jwtHeader = "eyJ0eXAiOiJKV1QiLCJhbGciOiJIUzI1NiJ9" // {"typ":"JWT","alg":"HS256"}
)

var (
	ErrTokenInvalid = errors.New("invalid token")
	ErrTokenExpired = errors.New("token expired")
	ErrTokenType    = errors.New("invalid token type")
)

// TokenClaims uses the original LiveMedia JSON types, including numeric IDs.
type TokenClaims struct {
	Sub int64  `json:"sub"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
	Typ string `json:"typ"`
	Aud string `json:"aud"`
	JTI int64  `json:"jti"`
	Iss string `json:"iss"`
}

// JWTService signs and verifies tokens. Callers use the refresh JTI for
// server-side session checks and revocation.
type JWTService struct {
	secret string
	now    func() time.Time
}

func NewJWTService(secret string) *JWTService {
	return &JWTService{secret: secret, now: time.Now}
}

func (s *JWTService) IssueRefresh(userID int64, issuer string) (string, TokenClaims, error) {
	return s.issue(userID, issuer, RefreshType, "", refreshTTL)
}

func (s *JWTService) VerifyRefresh(token string) (TokenClaims, error) {
	return s.verify(token, RefreshType)
}

func (s *JWTService) ExchangeRefresh(token string) (string, TokenClaims, error) {
	claims, err := s.VerifyRefresh(token)
	if err != nil {
		return "", TokenClaims{}, err
	}
	return s.issue(claims.Sub, claims.Iss, AccessType, claims.Aud, accessTTL)
}

func (s *JWTService) VerifyAccess(token string) (TokenClaims, error) {
	return s.verify(token, AccessType)
}

func (s *JWTService) issue(userID int64, issuer, typ, audience string, ttl time.Duration) (string, TokenClaims, error) {
	if s.secret == "" || userID < 1 {
		return "", TokenClaims{}, ErrTokenInvalid
	}
	var random [8]byte
	rand.Read(random[:])
	jti := int64(binary.BigEndian.Uint64(random[:])&(1<<62-1)) + 1
	if audience == "" {
		audience = strconv.FormatInt(jti, 10)
	}

	now := s.now()
	claims := TokenClaims{
		Sub: userID,
		Iat: now.Unix(),
		Exp: now.Add(ttl).Unix(),
		Typ: typ,
		Aud: audience,
		JTI: jti,
		Iss: issuer,
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", TokenClaims{}, err
	}
	message := jwtHeader + "." + base64.RawURLEncoding.EncodeToString(payload)
	signature := base64.RawURLEncoding.EncodeToString(s.signature(message, userID))
	return message + "." + signature, claims, nil
}

func (s *JWTService) verify(token, expectedType string) (TokenClaims, error) {
	header, rest, found := strings.Cut(token, ".")
	payload, signature, hasSignature := strings.Cut(rest, ".")
	if s.secret == "" || !found || !hasSignature {
		return TokenClaims{}, ErrTokenInvalid
	}
	headerJSON, err := base64.RawURLEncoding.Strict().DecodeString(header)
	if err != nil {
		return TokenClaims{}, ErrTokenInvalid
	}
	var metadata struct {
		Algorithm string `json:"alg"`
	}
	if err := json.Unmarshal(headerJSON, &metadata); err != nil || metadata.Algorithm != "HS256" {
		return TokenClaims{}, ErrTokenInvalid
	}
	payloadJSON, err := base64.RawURLEncoding.Strict().DecodeString(payload)
	if err != nil {
		return TokenClaims{}, ErrTokenInvalid
	}
	var claims TokenClaims
	if err := json.Unmarshal(payloadJSON, &claims); err != nil || claims.Sub < 1 {
		return TokenClaims{}, ErrTokenInvalid
	}
	actual, err := base64.RawURLEncoding.Strict().DecodeString(signature)
	if err != nil || !hmac.Equal(actual, s.signature(header+"."+payload, claims.Sub)) {
		return TokenClaims{}, ErrTokenInvalid
	}
	if claims.Typ != expectedType {
		return TokenClaims{}, ErrTokenType
	}
	audience, err := strconv.ParseInt(claims.Aud, 10, 64)
	if err != nil || audience < 1 || claims.JTI < 1 || claims.Iat <= 0 || claims.Exp <= claims.Iat ||
		(expectedType == RefreshType && audience != claims.JTI) {
		return TokenClaims{}, ErrTokenInvalid
	}
	if s.now().Unix() >= claims.Exp {
		return TokenClaims{}, ErrTokenExpired
	}
	return claims, nil
}

func (s *JWTService) signature(message string, userID int64) []byte {
	// LiveMedia uses the lowercase ASCII hex digest as its HS256 key.
	digest := md5.Sum([]byte(s.secret + ":" + strconv.FormatInt(userID, 10)))
	var key [2 * md5.Size]byte
	hex.Encode(key[:], digest[:])
	mac := hmac.New(sha256.New, key[:])
	mac.Write([]byte(message))
	return mac.Sum(nil)
}
