package app_test

import (
	"errors"
	"math"
	"os"
	"slices"
	"testing"

	"open_live_platform/internal/live"
	"open_live_platform/internal/store/postgres"
)

func TestPostgresRTCNIDsAcrossInstancesAndBroadcasts(t *testing.T) {
	config, db := postgresTestDatabase(t)
	ctx := t.Context()
	const (
		owner  int64 = 1
		legacy int64 = 9007199254740993
		viewer       = legacy + 1<<32
		other        = viewer + 1<<32
	)
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	schema, err := os.ReadFile("../store/postgres/migrations/001_core.sql")
	check(err)
	_, err = db.Exec(ctx, string(schema))
	check(err)
	_, err = db.Exec(ctx, `INSERT INTO users(id,email,account,password_salt,password_hash) VALUES
		($1,'host@example.org','host','',''),($2,'legacy@example.org','legacy','',''),
		($3,'viewer@example.org','viewer','',''),($4,'other@example.org','other','','')`, owner, legacy, viewer, other)
	check(err)
	_, err = db.Exec(ctx, "INSERT INTO rooms(id,owner_id,title) VALUES($1,$1,'Existing broadcast')", owner)
	check(err)
	_, err = db.Exec(ctx, "INSERT INTO room_members(room_id,user_id) VALUES($1,$2)", owner, legacy)
	check(err)

	open := func() *postgres.Store {
		t.Helper()
		store, err := postgres.Open(ctx, config.DatabaseURL)
		check(err)
		t.Cleanup(store.Shutdown)
		return store
	}
	first, peer := open(), open()
	initial, err := first.Info(ctx, owner)
	check(err)
	if initial.Session.LiveID == 0 || initial.ViewerNID != 0 || initial.MemberCount != 1 {
		t.Fatalf("upgraded room = %+v", initial)
	}
	anchor, err := first.JoinRoom(ctx, owner, owner, "")
	check(err)
	if anchor.ViewerNID != live.DefaultAnchorNID || anchor.MemberCount != 1 {
		t.Fatalf("anchor join = %+v", anchor)
	}
	if _, err := first.RTCNID(ctx, owner, viewer); !errors.Is(err, live.ErrNotMember) {
		t.Fatalf("renewal before join = %v, want %v", err, live.ErrNotMember)
	}
	legacyNID, err := peer.RTCNID(ctx, owner, legacy)
	check(err)
	legacyJoin, err := first.JoinRoom(ctx, owner, legacy, "")
	check(err)
	if legacyNID != live.DefaultAnchorNID+1 || legacyJoin.ViewerNID != legacyNID || legacyJoin.MemberCount != 1 {
		t.Fatalf("legacy RTC ID = %d, join = %+v", legacyNID, legacyJoin)
	}

	type joinResult struct {
		uid int64
		nid uint32
		err error
	}
	start := make(chan struct{})
	results := make(chan joinResult, 4)
	for _, store := range []*postgres.Store{first, peer} {
		for _, uid := range []int64{viewer, other} {
			go func() {
				<-start
				room, err := store.JoinRoom(ctx, owner, uid, "")
				results <- joinResult{uid, room.ViewerNID, err}
			}()
		}
	}
	close(start)
	assigned := make(map[int64]uint32)
	for range cap(results) {
		joined := <-results
		check(joined.err)
		if known, ok := assigned[joined.uid]; ok && joined.nid != known {
			t.Fatalf("cross-instance join for %d returned RTC IDs %d and %d", joined.uid, known, joined.nid)
		}
		assigned[joined.uid] = joined.nid
	}
	ids := []uint32{assigned[viewer], assigned[other]}
	slices.Sort(ids)
	maxAssigned := live.DefaultAnchorNID + 3
	if !slices.Equal(ids, []uint32{maxAssigned - 1, maxAssigned}) {
		t.Fatalf("concurrent join RTC IDs = %v", assigned)
	}
	check(peer.Leave(ctx, owner, viewer))
	if _, err := first.RTCNID(ctx, owner, viewer); !errors.Is(err, live.ErrNotMember) {
		t.Fatalf("renewal after leave = %v, want %v", err, live.ErrNotMember)
	}
	// A newly opened store has no in-process state from the original allocation.
	restarted := open()
	rejoined, err := restarted.JoinRoom(ctx, owner, viewer, "")
	check(err)
	if rejoined.ViewerNID != assigned[viewer] || rejoined.MemberCount != 3 {
		t.Fatalf("rejoin after restart = %+v, want RTC ID %d and 3 members", rejoined, assigned[viewer])
	}
	check(restarted.Close(ctx, owner, owner))
	newRoom, err := peer.Start(ctx, owner, "Next broadcast", "")
	check(err)
	joined, err := first.JoinRoom(ctx, owner, viewer, "")
	check(err)
	if newRoom.Session.LiveID == initial.Session.LiveID || joined.Session != newRoom.Session ||
		joined.ViewerNID != maxAssigned+1 || joined.MemberCount != 1 {
		t.Fatalf("joined next broadcast = %+v, want session %+v and RTC ID %d", joined, newRoom.Session, maxAssigned+1)
	}

	_, err = db.Exec(ctx, "UPDATE rooms SET last_viewer_nid=$2 WHERE id=$1", owner, int64(math.MaxUint32-1))
	check(err)
	last, err := peer.JoinRoom(ctx, owner, other, "")
	check(err)
	if last.ViewerNID != math.MaxUint32 || last.MemberCount != 2 {
		t.Fatalf("last available RTC ID = %+v", last)
	}
	if _, err := first.JoinRoom(ctx, owner, legacy, ""); !errors.Is(err, live.ErrViewerNIDExhausted) {
		t.Fatalf("join after exhaustion = %v, want %v", err, live.ErrViewerNIDExhausted)
	}
	if _, err := peer.RTCNID(ctx, owner, legacy); !errors.Is(err, live.ErrNotMember) {
		t.Fatalf("membership after failed join = %v, want %v", err, live.ErrNotMember)
	}
}
