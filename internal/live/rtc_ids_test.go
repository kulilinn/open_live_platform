package live

import (
	"encoding/json"
	"math"
	"testing"
)

func TestRTCNIDsAcrossBroadcasts(t *testing.T) {
	store := NewStore()
	ctx := t.Context()
	const (
		owner    int64 = 42
		viewer   int64 = 9007199254740993
		other          = viewer + 1<<32
		password       = "room password"
	)
	first, err := store.Start(ctx, owner, "First broadcast", password)
	requireError(t, err, nil)
	_, err = store.RTCNID(ctx, owner+1, viewer)
	requireError(t, err, ErrRoomNotFound)
	_, err = store.RTCNID(ctx, owner, viewer)
	requireError(t, err, ErrNotMember)
	_, err = store.JoinRoom(ctx, owner, viewer, "wrong")
	requireError(t, err, ErrInvalidRoomPassword)

	assertNID := func(user int64, want uint32) {
		t.Helper()
		nid, err := store.RTCNID(ctx, owner, user)
		requireError(t, err, nil)
		if nid != want {
			t.Fatalf("RTCNID(%d) = %d, want %d", user, nid, want)
		}
	}
	join := func(user int64, password string, nid uint32, members int) Room {
		t.Helper()
		room, err := store.JoinRoom(ctx, owner, user, password)
		requireError(t, err, nil)
		if room.ViewerNID != nid || room.MemberCount != members {
			t.Fatalf("JoinRoom(%d) = %+v, want RTC ID %d and %d members", user, room, nid, members)
		}
		return room
	}
	join(owner, "", DefaultAnchorNID, 0)
	assertNID(owner, DefaultAnchorNID)
	joined := join(viewer, password, DefaultAnchorNID+1, 1)
	assertNID(viewer, joined.ViewerNID)
	info, err := store.Info(ctx, owner)
	requireError(t, err, nil)
	want := first
	want.MemberCount = 1
	if info != want || joined.Session != first.Session {
		t.Fatalf("snapshot = %+v, join = %+v, want %+v", info, joined, want)
	}
	infoJSON, err := json.Marshal(info)
	requireError(t, err, nil)
	joinedJSON, err := json.Marshal(joined)
	requireError(t, err, nil)
	if string(infoJSON) != string(joinedJSON) {
		t.Fatalf("join snapshot marshaled to %s, want %s", joinedJSON, infoJSON)
	}

	requireError(t, store.Leave(ctx, owner, viewer), nil)
	_, err = store.RTCNID(ctx, owner, viewer)
	requireError(t, err, ErrNotMember)
	join(other, password, DefaultAnchorNID+2, 1)
	join(viewer, password, joined.ViewerNID, 2)
	join(viewer, password, joined.ViewerNID, 2)
	requireError(t, store.Kick(ctx, owner, owner, viewer), nil)
	_, err = store.RTCNID(ctx, owner, viewer)
	requireError(t, err, ErrNotMember)
	_, err = store.JoinRoom(ctx, owner, viewer, password)
	requireError(t, err, ErrKicked)

	requireError(t, store.Close(ctx, owner, owner), nil)
	_, err = store.RTCNID(ctx, owner, owner)
	requireError(t, err, ErrRoomClosed)
	second, err := store.Start(ctx, owner, "Second broadcast", "")
	requireError(t, err, nil)
	resumed := join(viewer, "", DefaultAnchorNID+3, 1)
	if resumed.Session != second.Session || resumed.Session.LiveID == joined.Session.LiveID {
		t.Fatalf("joined new broadcast = %+v, want session %+v", resumed, second.Session)
	}
}

func TestRTCNIDExhaustionPreservesMembership(t *testing.T) {
	store := NewStore()
	ctx := t.Context()
	const owner, viewer, other = 42, 43, 44
	_, err := store.Start(ctx, owner, "Live", "")
	requireError(t, err, nil)
	store.rooms[owner].lastViewerNID = math.MaxUint32 - 1
	last, err := store.JoinRoom(ctx, owner, viewer, "")
	requireError(t, err, nil)
	if last.ViewerNID != math.MaxUint32 {
		t.Fatalf("last RTC ID = %d, want %d", last.ViewerNID, uint32(math.MaxUint32))
	}
	_, err = store.JoinRoom(ctx, owner, other, "")
	requireError(t, err, ErrViewerNIDExhausted)
	_, err = store.RTCNID(ctx, owner, other)
	requireError(t, err, ErrNotMember)
	nid, err := store.RTCNID(ctx, owner, viewer)
	requireError(t, err, nil)
	if nid != last.ViewerNID {
		t.Fatalf("RTC ID for existing member = %d, want %d", nid, last.ViewerNID)
	}
}
