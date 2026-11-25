package live

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"

	"open_live_platform/internal/auth"
)

// MaxRoomPasswordAttempts is the number of failed attempts allowed per viewer and broadcast.
const MaxRoomPasswordAttempts = 5

const roomPasswordSaltSize = 16

// HashRoomPassword stores a random salt followed by the derived password key.
// An empty password opens the room to all viewers.
func HashRoomPassword(password string) ([]byte, error) {
	if password == "" {
		return nil, nil
	}

	var salt [roomPasswordSaltSize]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return nil, err
	}
	key, err := auth.PasswordKey(password, salt[:])
	if err != nil {
		return nil, err
	}
	return append(salt[:], key...), nil
}

// RoomPasswordMatches accepts every password for a room with no password hash.
func RoomPasswordMatches(hash []byte, password string) bool {
	if len(hash) == 0 {
		return true
	}
	if len(hash) != roomPasswordSaltSize+sha256.Size || password == "" {
		return false
	}

	key, err := auth.PasswordKey(password, hash[:roomPasswordSaltSize])
	return err == nil && subtle.ConstantTimeCompare(key, hash[roomPasswordSaltSize:]) == 1
}
