package auth

import (
	"crypto/subtle"
	"encoding/base64"
	"strings"

	"golang.org/x/crypto/argon2"
)

// LiveMedia generated Argon2id PHC strings using RustCrypto's registration
// settings or its crate defaults. Parse the source formats before importing.
type LegacyPassword struct {
	memory uint32
	salt   []byte
	hash   []byte
}

func ParseLegacyPassword(encoded string) (LegacyPassword, bool) {
	fields := strings.Split(encoded, "$")
	if len(fields) != 6 || fields[0] != "" || fields[1] != "argon2id" || fields[2] != "v=19" {
		return LegacyPassword{}, false
	}
	var memory uint32
	switch fields[3] {
	case "m=4096,t=2,p=1":
		memory = 4096
	case "m=19456,t=2,p=1":
		memory = 19456
	default:
		return LegacyPassword{}, false
	}
	salt, err := base64.RawStdEncoding.DecodeString(fields[4])
	if err != nil || len(salt) < 8 || len(salt) > 64 {
		return LegacyPassword{}, false
	}
	hash, err := base64.RawStdEncoding.DecodeString(fields[5])
	if err != nil || len(hash) != 32 {
		return LegacyPassword{}, false
	}
	return LegacyPassword{memory, salt, hash}, true
}

func (p LegacyPassword) Matches(password string) bool {
	key := argon2.IDKey([]byte(password), p.salt, 2, p.memory, 1, 32)
	return subtle.ConstantTimeCompare(key, p.hash) == 1
}
