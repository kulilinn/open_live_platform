package auth

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	RefreshType = "REFRESH"
	AccessType  = "ACCESS"

	refreshTTL = 24 * time.Hour
	accessTTL  = 30 * time.Minute
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

// jwtClaims implements jwt.Claims with LiveMedia's JSON field types.
type jwtClaims TokenClaims

func (c jwtClaims) GetExpirationTime() (*jwt.NumericDate, error) {
	return jwt.NewNumericDate(time.Unix(c.Exp, 0)), nil
}

func (c jwtClaims) GetIssuedAt() (*jwt.NumericDate, error) {
	return jwt.NewNumericDate(time.Unix(c.Iat, 0)), nil
}

func (jwtClaims) GetNotBefore() (*jwt.NumericDate, error) { return nil, nil }
func (c jwtClaims) GetIssuer() (string, error)            { return c.Iss, nil }
func (c jwtClaims) GetSubject() (string, error)           { return strconv.FormatInt(c.Sub, 10), nil }
func (c jwtClaims) GetAudience() (jwt.ClaimStrings, error) {
	return jwt.ClaimStrings{c.Aud}, nil
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
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwtClaims(claims)).SignedString(s.signingKey(userID))
	if err != nil {
		return "", TokenClaims{}, err
	}
	return token, claims, nil
}

func (s *JWTService) verify(token, expectedType string) (TokenClaims, error) {
	if s.secret == "" {
		return TokenClaims{}, ErrTokenInvalid
	}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithStrictDecoding(),
		jwt.WithoutClaimsValidation(), // Apply LiveMedia's claim checks after signature verification.
	)
	var claims jwtClaims
	_, err := parser.ParseWithClaims(token, &claims, func(_ *jwt.Token) (any, error) {
		if claims.Sub < 1 {
			return nil, ErrTokenInvalid
		}
		return s.signingKey(claims.Sub), nil
	})
	if err != nil {
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
	return TokenClaims(claims), nil
}

func (s *JWTService) signingKey(userID int64) []byte {
	// LiveMedia uses the lowercase ASCII hex digest as its HS256 key.
	digest := md5.Sum([]byte(s.secret + ":" + strconv.FormatInt(userID, 10)))
	var key [2 * md5.Size]byte
	hex.Encode(key[:], digest[:])
	return key[:]
}
