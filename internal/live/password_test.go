package live

import (
	"bytes"
	"strings"
	"testing"
)

func TestHashRoomPassword(t *testing.T) {
	empty, err := HashRoomPassword("")
	if err != nil || empty != nil || !RoomPasswordMatches(empty, "any password") {
		t.Fatalf("HashRoomPassword for public room = %v, %v", empty, err)
	}
	prefix := strings.Repeat("long password ", 12)
	password := prefix + "one"
	first, err := HashRoomPassword(password)
	requireError(t, err, nil)
	second, err := HashRoomPassword(password)
	requireError(t, err, nil)
	if bytes.Equal(first, second) {
		t.Fatal("two hashes of the same password are identical")
	}
	if !RoomPasswordMatches(first, password) || !RoomPasswordMatches(second, password) {
		t.Fatal("password failed to match its hash")
	}
	if RoomPasswordMatches(first, prefix+"two") || RoomPasswordMatches(first, "") {
		t.Fatal("wrong password matched")
	}
	if RoomPasswordMatches(first[:1], password) {
		t.Fatal("malformed hash matched")
	}
}
