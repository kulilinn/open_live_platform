package auth

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"
	"unicode/utf8"
)

// LegacySessionRecord is one key, raw value and absolute expiry exported from Rust Redis.
type LegacySessionRecord struct {
	Key       string    `json:"key"`
	Value     []byte    `json:"value_base64"`
	ExpiresAt time.Time `json:"expires_at"`
}

type LegacyJWTSession struct {
	UserID    int64
	Email     string
	FamilyID  int64
	ExpiresAt time.Time
}

// ParseLegacyJWTSession decodes the bincode 1.x representation of Rust AuthUser.
func ParseLegacyJWTSession(record LegacySessionRecord, appName string) (LegacyJWTSession, error) {
	invalid := errors.New("invalid Rust AuthUser value")
	r := bytes.NewReader(record.Value)
	var session LegacyJWTSession
	if err := binary.Read(r, binary.LittleEndian, &session.UserID); err != nil || session.UserID < 1 {
		return LegacyJWTSession{}, invalid
	}
	if appName == "" || record.Key != fmt.Sprintf("{%d}|auth_user|%s", session.UserID, appName) {
		return LegacyJWTSession{}, errors.New("Redis key does not match AuthUser ID and application name")
	}
	email, ok := readBincodeString(r)
	if !ok {
		return LegacyJWTSession{}, invalid
	}
	session.Email = email
	// The access JTI precedes the refresh JTI. Rust and Go bind access
	// tokens to the refresh family, so no access JTIs are carried forward.
	if _, err := r.Seek(8, io.SeekCurrent); err != nil {
		return LegacyJWTSession{}, invalid
	}
	if err := binary.Read(r, binary.LittleEndian, &session.FamilyID); err != nil || session.FamilyID < 1 {
		return LegacyJWTSession{}, invalid
	}
	lastAccess, err := r.ReadByte()
	if err != nil || lastAccess > 1 {
		return LegacyJWTSession{}, invalid
	}
	if lastAccess == 1 {
		if _, err := r.Seek(8, io.SeekCurrent); err != nil {
			return LegacyJWTSession{}, invalid
		}
	}
	refreshedAt, err := r.ReadByte()
	if err != nil || refreshedAt > 1 {
		return LegacyJWTSession{}, invalid
	}
	if refreshedAt == 1 {
		value, ok := readBincodeString(r)
		if _, err := time.Parse(time.RFC3339Nano, value); !ok || err != nil {
			return LegacyJWTSession{}, invalid
		}
	}
	// The final byte holds auth_version, which Rust accepts from a u8 header.
	if r.Len() != 1 {
		return LegacyJWTSession{}, invalid
	}
	if record.ExpiresAt.IsZero() || record.ExpiresAt.UnixMilli() <= 0 {
		return LegacyJWTSession{}, errors.New("Redis expiry must be an absolute timestamp")
	}
	session.ExpiresAt = record.ExpiresAt
	return session, nil
}

func readBincodeString(r *bytes.Reader) (string, bool) {
	var size uint64
	if err := binary.Read(r, binary.LittleEndian, &size); err != nil || size > uint64(r.Len()) {
		return "", false
	}
	value := make([]byte, int(size))
	if size != 0 {
		_, _ = r.Read(value)
	}
	return string(value), utf8.Valid(value)
}
