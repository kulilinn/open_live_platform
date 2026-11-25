package app_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"

	"open_live_platform/internal/app"
	"open_live_platform/internal/live"
)

func postgresTestDatabase(t *testing.T) (app.Config, *pgx.Conn) {
	t.Helper()
	dsn := os.Getenv("OPEN_LIVE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set OPEN_LIVE_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := t.Context()
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(context.Background()) })
	schema := "test_live_" + strings.ToLower(rand.Text())
	if _, err := db.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	if _, err := db.Exec(ctx, "SET search_path TO "+schema); err != nil {
		t.Fatal(err)
	}
	databaseURL, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := databaseURL.Query()
	q.Set("search_path", schema)
	databaseURL.RawQuery = q.Encode()
	return app.Config{DatabaseURL: databaseURL.String(), RelaySecret: rand.Text()}, db
}

func TestPostgresRoomListing(t *testing.T) {
	config, _ := postgresTestDatabase(t)
	ctx := t.Context()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	platform, err := app.New(ctx, config)
	check(err)
	defer platform.Close()
	store := platform.Rooms

	start := func(name, password string) live.Room {
		t.Helper()
		owner, err := platform.Accounts.SignUp(ctx, name+"@example.org", "password", name)
		check(err)
		room, err := store.Start(ctx, owner.User.ID, name, password)
		check(err)
		return room
	}
	assertList := func(filter live.RoomFilter, wantPages int64, wantRooms ...live.Room) {
		t.Helper()
		rooms, pages, err := store.ListRooms(ctx, filter)
		if err != nil || rooms == nil || pages != wantPages || !slices.Equal(rooms, wantRooms) {
			t.Fatalf("room list = %+v, pages = %d, error = %v; want %+v, pages = %d", rooms, pages, err, wantRooms, wantPages)
		}
	}
	online, offline := true, false
	assertList(live.RoomFilter{Page: 1, PageSize: 2}, 0)

	first := start("first", "")
	check(store.Close(ctx, first.ID, first.OwnerID))
	first.Online = false
	second := start("second", "room password")
	third := start("third", "")
	check(store.Join(ctx, second.ID, first.OwnerID, "room password"))
	second.MemberCount = 1

	assertList(live.RoomFilter{Page: 1, PageSize: 2}, 2, second, third)
	assertList(live.RoomFilter{Page: 2, PageSize: 2}, 2, first)
	assertList(live.RoomFilter{Page: 3, PageSize: 2}, 2)
	assertList(live.RoomFilter{Online: &online, Page: 2, PageSize: 1}, 2, third)
	assertList(live.RoomFilter{Online: &offline, Page: 2, PageSize: 1}, 1)
	assertList(live.RoomFilter{Online: &offline, Page: 1, PageSize: 2}, 1, first)
	assertList(live.RoomFilter{Page: 4}, 3)
	assertList(live.RoomFilter{Online: &online, Page: 1}, 2)

	rooms, err := store.Rooms(ctx)
	check(err)
	if !slices.Equal(rooms, []live.Room{first, second, third}) {
		t.Fatalf("administrative room list = %+v, want ID order", rooms)
	}

	check(store.Close(ctx, second.ID, second.OwnerID))
	second.Online, second.IsLock, second.MemberCount = false, false, 0
	reopened, err := store.Start(ctx, first.OwnerID, "reopened", "")
	check(err)
	assertList(live.RoomFilter{Online: &online, Page: 1, PageSize: 2}, 1, reopened, third)
	assertList(live.RoomFilter{Online: &offline, Page: 1, PageSize: 2}, 1, second)
}

func TestPostgresRestartAndRelay(t *testing.T) {
	config, db := postgresTestDatabase(t)
	ctx := t.Context()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	start := func() *app.App {
		t.Helper()
		platform, err := app.New(ctx, config)
		check(err)
		return platform
	}

	first := start()
	var viewerToken string
	var roomID, viewerID int64
	func() {
		defer first.Close()
		owner, err := first.Accounts.SignUp(ctx, "host@example.org", "password", "host")
		check(err)
		viewer, err := first.Accounts.SignUp(ctx, "viewer@example.org", "password", "viewer")
		check(err)
		room, err := first.Rooms.Start(ctx, owner.User.ID, "Evening show", "")
		check(err)
		viewerToken, roomID, viewerID = viewer.Token, room.ID, viewer.User.ID
		for range 2 {
			check(first.Rooms.Join(ctx, roomID, viewerID, ""))
		}
		server := httptest.NewServer(first.Server.Handler)
		defer server.Close()
		issue, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/device/qrcode", nil)
		check(err)
		issue.Header.Set("Authorization", "Bearer "+owner.Token)
		ticket, err := server.Client().Do(issue)
		check(err)
		defer ticket.Body.Close()
		if ticket.StatusCode != http.StatusOK {
			t.Fatalf("issue relay ticket returned %s", ticket.Status)
		}
		var connector struct{ URL string }
		check(json.NewDecoder(ticket.Body).Decode(&connector))
		ws, _, err := websocket.DefaultDialer.DialContext(ctx, connector.URL, nil)
		check(err)
		defer ws.Close()
		req, err := http.NewRequestWithContext(ctx, "POST", server.URL+"/api/live/room/chat", strings.NewReader(fmt.Sprintf(`{"room_id":%d,"text":"hello"}`, roomID)))
		check(err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+viewerToken)
		res, err := server.Client().Do(req)
		check(err)
		res.Body.Close()
		if res.StatusCode != http.StatusNoContent {
			t.Fatalf("chat returned %s", res.Status)
		}
		ws.SetReadDeadline(time.Now().Add(5 * time.Second))
		var event live.Event
		check(ws.ReadJSON(&event))
		if event != (live.Event{Type: "chat", RoomID: roomID, UserID: viewerID, Text: "hello"}) {
			t.Fatalf("broadcast = %+v", event)
		}
		check(first.Rooms.Mute(ctx, roomID, owner.User.ID, viewerID, time.Hour))
	}()

	second := start()
	defer second.Close()
	req := httptest.NewRequest("GET", fmt.Sprintf("/api/live/room/info/%d", roomID), nil)
	req.Header.Set("Authorization", "Bearer "+viewerToken)
	rec := httptest.NewRecorder()
	second.Server.Handler.ServeHTTP(rec, req)
	var room live.Room
	if rec.Code != http.StatusOK {
		t.Fatalf("restored session returned %d", rec.Code)
	}
	check(json.Unmarshal(rec.Body.Bytes(), &room))
	if !room.Online || room.IsLock || room.Title != "Evening show" || room.MemberCount != 1 || room.ID != roomID {
		t.Fatalf("restored room = %+v", room)
	}
	if members, err := second.Rooms.Members(ctx, roomID); err != nil || !slices.Equal(members, []int64{viewerID}) {
		t.Fatalf("restored members = %v, %v", members, err)
	}
	if err := second.Rooms.Send(ctx, roomID, viewerID, "muted"); !errors.Is(err, live.ErrMuted) {
		t.Fatalf("mute after restart = %v", err)
	}
	var sender int64
	var text string
	if err := db.QueryRow(ctx, "SELECT user_id, text FROM room_messages WHERE room_id=$1 ORDER BY id DESC LIMIT 1", roomID).Scan(&sender, &text); err != nil || sender != viewerID || text != "hello" {
		t.Fatalf("persisted message = %d %q, %v", sender, text, err)
	}
}

func TestPostgresRoomPasswordUpgradeAndAttempts(t *testing.T) {
	config, db := postgresTestDatabase(t)
	ctx := t.Context()
	check := func(got, want error) {
		t.Helper()
		if !errors.Is(got, want) {
			t.Fatalf("error = %v, want %v", got, want)
		}
	}
	legacySchema, err := os.ReadFile("../store/postgres/migrations/001_core.sql")
	check(err, nil)
	_, err = db.Exec(ctx, string(legacySchema))
	check(err, nil)
	_, err = db.Exec(ctx, `INSERT INTO users(email,account,password_salt,password_hash) VALUES
		('host@example.org','host','\x01','\x01'),
		('viewer@example.org','viewer','\x01','\x01'),
		('other@example.org','other','\x01','\x01');
		INSERT INTO rooms(id,owner_id,title) VALUES (1,1,'Existing room');
		INSERT INTO room_members(room_id,user_id) VALUES (1,2)`)
	check(err, nil)

	const (
		ownerID  = 1
		viewerID = 2
		otherID  = 3
		password = "broadcast password"
	)
	open := func() *app.App {
		t.Helper()
		platform, err := app.New(ctx, config)
		check(err, nil)
		return platform
	}
	func() {
		platform := open()
		defer platform.Close()
		rooms := platform.Rooms
		legacy, err := rooms.Info(ctx, ownerID)
		check(err, nil)
		if !legacy.Online || legacy.IsLock || legacy.Title != "Existing room" || legacy.MemberCount != 1 {
			t.Fatalf("upgraded room = %+v", legacy)
		}
		check(rooms.Join(ctx, ownerID, viewerID, "unused"), nil)
		_, err = rooms.Start(ctx, ownerID, "Ignored title", password)
		check(err, live.ErrRoomAlreadyOnline)
		check(rooms.Close(ctx, ownerID, ownerID), nil)

		room, err := rooms.Start(ctx, ownerID, "Private broadcast", password)
		check(err, nil)
		if !room.Online || !room.IsLock || room.MemberCount != 0 {
			t.Fatalf("private broadcast = %+v", room)
		}
		var hash []byte
		check(db.QueryRow(ctx, "SELECT password_hash FROM rooms WHERE id=$1", ownerID).Scan(&hash), nil)
		if len(hash) == 0 || string(hash) == password || !live.RoomPasswordMatches(hash, password) {
			t.Fatal("room password was not persisted as a verifiable hash")
		}
		for _, retryPassword := range []string{"", password} {
			retry, err := rooms.Start(ctx, ownerID, "Ignored title", retryPassword)
			check(err, nil)
			if retry != room {
				t.Fatalf("repeated start = %+v, want %+v", retry, room)
			}
		}
		_, err = rooms.Start(ctx, ownerID, "Ignored title", "new password")
		check(err, live.ErrRoomAlreadyOnline)
		check(rooms.Join(ctx, ownerID, ownerID, ""), nil)
		for range live.MaxRoomPasswordAttempts - 2 {
			check(rooms.Join(ctx, ownerID, viewerID, "wrong"), live.ErrInvalidRoomPassword)
		}
	}()

	first, second := open(), open()
	defer first.Close()
	defer second.Close()
	room, err := second.Rooms.Info(ctx, ownerID)
	check(err, nil)
	rooms, err := second.Rooms.Rooms(ctx)
	check(err, nil)
	if !room.IsLock || room.MemberCount != 0 || !slices.Equal(rooms, []live.Room{room}) {
		t.Fatalf("private room after restart = %+v, list = %+v", room, rooms)
	}

	results := make(chan error, 3)
	for _, store := range []live.RoomStore{first.Rooms, second.Rooms, first.Rooms} {
		go func() {
			results <- store.Join(ctx, ownerID, viewerID, "wrong")
		}()
	}
	invalid, locked := 0, 0
	for range cap(results) {
		switch err := <-results; {
		case errors.Is(err, live.ErrInvalidRoomPassword):
			invalid++
		case errors.Is(err, live.ErrRoomPasswordLocked):
			locked++
		default:
			t.Errorf("concurrent join error = %v", err)
		}
	}
	if invalid != 2 || locked != 1 {
		t.Fatalf("concurrent joins = %d invalid, %d locked; want 2 invalid, 1 locked", invalid, locked)
	}
	check(second.Rooms.Join(ctx, ownerID, viewerID, password), live.ErrRoomPasswordLocked)

	check(second.Rooms.Join(ctx, ownerID, otherID, ""), live.ErrInvalidRoomPassword)
	check(second.Rooms.Join(ctx, ownerID, otherID, password), nil)
	var failedUsers int
	check(db.QueryRow(ctx, "SELECT count(*) FROM room_password_attempts WHERE room_id=$1", ownerID).Scan(&failedUsers), nil)
	if failedUsers != 1 {
		t.Fatalf("viewers with failed password attempts after successful join = %d, want 1", failedUsers)
	}
	check(first.Rooms.Join(ctx, ownerID, otherID, "wrong"), live.ErrInvalidRoomPassword)
	members, err := first.Rooms.Members(ctx, ownerID)
	check(err, nil)
	if !slices.Equal(members, []int64{otherID}) {
		t.Fatalf("private room members = %v, want [%d]", members, otherID)
	}

	check(first.Rooms.Close(ctx, ownerID, ownerID), nil)
	room, err = second.Rooms.Info(ctx, ownerID)
	check(err, nil)
	if room.Online || room.IsLock || room.MemberCount != 0 {
		t.Fatalf("closed private room = %+v", room)
	}
	check(db.QueryRow(ctx, "SELECT count(*) FROM room_password_attempts WHERE room_id=$1", ownerID).Scan(&failedUsers), nil)
	if failedUsers != 0 {
		t.Fatalf("viewers with failed password attempts after close = %d, want 0", failedUsers)
	}
	_, err = second.Rooms.Start(ctx, ownerID, "Next broadcast", password)
	check(err, nil)
	check(first.Rooms.Join(ctx, ownerID, viewerID, password), nil)
}

func TestPostgresUpdateRoomSettingsAndRestart(t *testing.T) {
	config, db := postgresTestDatabase(t)
	ctx := t.Context()
	check := func(got, want error) {
		t.Helper()
		if !errors.Is(got, want) {
			t.Fatalf("error = %v, want %v", got, want)
		}
	}
	open := func() *app.App {
		t.Helper()
		platform, err := app.New(ctx, config)
		check(err, nil)
		return platform
	}
	passwordHash := func(id int64) []byte {
		t.Helper()
		var hash []byte
		check(db.QueryRow(ctx, "SELECT password_hash FROM rooms WHERE id=$1", id).Scan(&hash), nil)
		return hash
	}

	oldPassword, newPassword := "initial room password", "new room password"
	newTitle, empty := "  New title  ", ""
	var ownerID, memberID, blockedID, freshID, kickedID int64
	var updated live.Room
	func() {
		platform := open()
		defer platform.Close()
		signUp := func(name string) int64 {
			t.Helper()
			account, err := platform.Accounts.SignUp(ctx, name+"@example.org", "password", name)
			check(err, nil)
			return account.User.ID
		}
		ownerID, memberID, blockedID, freshID, kickedID = signUp("host"), signUp("member"), signUp("blocked"), signUp("fresh"), signUp("kicked")
		rooms := platform.Rooms
		_, err := rooms.UpdateSettings(ctx, ownerID, &newTitle, &newPassword)
		check(err, live.ErrRoomNotFound)
		_, err = rooms.Start(ctx, ownerID, "Initial title", oldPassword)
		check(err, nil)
		check(rooms.Join(ctx, ownerID, memberID, oldPassword), nil)
		check(rooms.SetAdmin(ctx, ownerID, ownerID, memberID, true), nil)
		check(rooms.Mute(ctx, ownerID, ownerID, memberID, time.Hour), nil)
		check(rooms.Join(ctx, ownerID, kickedID, oldPassword), nil)
		check(rooms.Kick(ctx, ownerID, ownerID, kickedID), nil)
		for range live.MaxRoomPasswordAttempts {
			check(rooms.Join(ctx, ownerID, blockedID, "wrong"), live.ErrInvalidRoomPassword)
		}
		events, unsubscribe, err := rooms.Subscribe(ctx, ownerID, memberID)
		check(err, nil)
		defer unsubscribe()

		originalHash := passwordHash(ownerID)
		room, err := rooms.UpdateSettings(ctx, ownerID, &empty, nil)
		check(err, nil)
		if room.Title != "" || !room.IsLock || !bytes.Equal(passwordHash(ownerID), originalHash) {
			t.Fatalf("title-only update = %+v", room)
		}
		unchanged, err := rooms.UpdateSettings(ctx, ownerID, nil, nil)
		check(err, nil)
		if unchanged != room {
			t.Fatalf("update with no fields = %+v, want %+v", unchanged, room)
		}
		updated, err = rooms.UpdateSettings(ctx, ownerID, &newTitle, &newPassword)
		check(err, nil)
		want := live.Room{ID: ownerID, OwnerID: ownerID, Title: newTitle, Online: true, IsLock: true, MemberCount: 1}
		if updated != want {
			t.Fatalf("updated room = %+v, want %+v", updated, want)
		}
		rotatedHash := passwordHash(ownerID)
		if !live.RoomPasswordMatches(rotatedHash, newPassword) || live.RoomPasswordMatches(rotatedHash, oldPassword) {
			t.Fatal("updated password hash does not match the new password exclusively")
		}
		_, err = rooms.UpdateSettings(ctx, ownerID, nil, &newPassword)
		check(err, nil)
		if bytes.Equal(passwordHash(ownerID), rotatedHash) {
			t.Fatal("updating the password reused its old hash")
		}
		check(rooms.Send(ctx, ownerID, ownerID, "still live"), nil)
		select {
		case event := <-events:
			if event.Type != "chat" || event.Text != "still live" {
				t.Fatalf("subscriber received %+v", event)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("subscriber did not receive chat after settings change")
		}
	}()

	platform, peer := open(), open()
	defer platform.Close()
	defer peer.Close()
	rooms := platform.Rooms
	restored, err := rooms.Info(ctx, ownerID)
	check(err, nil)
	if restored != updated {
		t.Fatalf("restored room = %+v, want %+v", restored, updated)
	}
	admins, err := rooms.Admins(ctx, ownerID)
	check(err, nil)
	if !slices.Equal(admins, []int64{memberID}) {
		t.Fatalf("restored administrators = %v, want [%d]", admins, memberID)
	}
	check(rooms.Send(ctx, ownerID, memberID, "muted"), live.ErrMuted)
	check(rooms.Join(ctx, ownerID, kickedID, newPassword), live.ErrKicked)
	check(rooms.Join(ctx, ownerID, blockedID, newPassword), live.ErrRoomPasswordLocked)
	check(rooms.Join(ctx, ownerID, freshID, oldPassword), live.ErrInvalidRoomPassword)
	check(rooms.Join(ctx, ownerID, freshID, newPassword), nil)
	check(rooms.Join(ctx, ownerID, freshID, oldPassword), live.ErrInvalidRoomPassword)
	_, err = rooms.Start(ctx, freshID, "Other broadcast", oldPassword)
	check(err, nil)
	check(rooms.Join(ctx, freshID, blockedID, "wrong"), live.ErrInvalidRoomPassword)

	joinResult := make(chan error, 1)
	go func() { joinResult <- peer.Rooms.Join(ctx, ownerID, freshID, oldPassword) }()
	public, err := rooms.UpdateSettings(ctx, ownerID, nil, &empty)
	check(err, nil)
	if err := <-joinResult; err != nil && !errors.Is(err, live.ErrInvalidRoomPassword) {
		t.Fatalf("join during password removal = %v", err)
	}
	if !public.Online || public.IsLock || public.Title != newTitle || public.MemberCount != 2 || passwordHash(ownerID) != nil {
		t.Fatalf("unlocked room = %+v", public)
	}
	var cleared, unrelated int
	check(db.QueryRow(ctx, `SELECT count(*) FILTER (WHERE room_id=$1), count(*) FILTER (WHERE room_id=$2)
		FROM room_password_attempts`, ownerID, freshID).Scan(&cleared, &unrelated), nil)
	if cleared != 0 || unrelated != 1 {
		t.Fatalf("remaining attempt rows for updated room = %d, other room = %d", cleared, unrelated)
	}
	_, err = rooms.UpdateSettings(ctx, ownerID, nil, &oldPassword)
	check(err, nil)
	check(peer.Rooms.Join(ctx, ownerID, blockedID, oldPassword), nil)

	closeResult := make(chan error, 1)
	go func() { closeResult <- peer.Rooms.Close(ctx, ownerID, ownerID) }()
	_, err = rooms.UpdateSettings(ctx, ownerID, nil, &newPassword)
	if err != nil && !errors.Is(err, live.ErrRoomClosed) {
		t.Fatalf("update during close = %v", err)
	}
	check(<-closeResult, nil)
	_, err = rooms.UpdateSettings(ctx, ownerID, &empty, &newPassword)
	check(err, live.ErrRoomClosed)
	closed, err := rooms.Info(ctx, ownerID)
	check(err, nil)
	if closed.Online || closed.IsLock || closed.MemberCount != 0 || closed.Title != newTitle {
		t.Fatalf("closed room = %+v", closed)
	}
}

func TestPostgresChatHistoryUpgradeAndRestart(t *testing.T) {
	config, db := postgresTestDatabase(t)
	ctx := t.Context()
	check := func(got, want error) {
		t.Helper()
		if !errors.Is(got, want) {
			t.Fatalf("error = %v, want %v", got, want)
		}
	}
	for _, migration := range []string{"001_core.sql", "002_room_passwords.sql"} {
		sql, err := os.ReadFile("../store/postgres/migrations/" + migration)
		check(err, nil)
		_, err = db.Exec(ctx, string(sql))
		check(err, nil)
	}
	_, err := db.Exec(ctx, `INSERT INTO users(email,account,password_salt,password_hash) VALUES
		('host@example.org','host','\x01','\x01'),
		('viewer@example.org','viewer','\x01','\x01'),
		('other@example.org','other','\x01','\x01');
		INSERT INTO rooms(id,owner_id,title,online,updated_at) VALUES
		(1,1,'Existing broadcast',true,now()-interval '3 hours'),
		(2,2,'Closed broadcast',false,now()-interval '3 hours');
		INSERT INTO room_members(room_id,user_id) VALUES (1,2);
		INSERT INTO room_messages(room_id,user_id,text,created_at) VALUES
		(1,1,'previous broadcast',now()-interval '4 hours'),
		(1,1,'at last update',now()-interval '3 hours'),
		(1,2,'existing chat',now()-interval '2 hours'),
		(2,2,'closed broadcast chat',now()-interval '1 hour')`)
	check(err, nil)

	const (
		ownerID  = 1
		viewerID = 2
		otherID  = 3
		payload  = `[{"userId":9007199254740993,"message":"legacy chat"}]`
	)
	open := func() *app.App {
		t.Helper()
		platform, err := app.New(ctx, config)
		check(err, nil)
		return platform
	}
	assertHistory := func(rooms live.RoomStore, roomID, userID int64, page, size uint64, want ...string) []live.ChatMessage {
		t.Helper()
		messages, err := rooms.ChatHistory(ctx, roomID, userID, page, size)
		check(err, nil)
		var texts []string
		for _, message := range messages {
			if message.ID <= 0 || message.RoomID != roomID || message.CreatedAt.IsZero() {
				t.Fatalf("invalid chat history record: %+v", message)
			}
			texts = append(texts, message.Text)
		}
		if messages == nil || !slices.Equal(texts, want) {
			t.Fatalf("chat history = %v, want %v", texts, want)
		}
		return messages
	}
	var sentID int64
	func() {
		platform := open()
		defer platform.Close()
		rooms := platform.Rooms
		assertHistory(rooms, ownerID, ownerID, 0, 10, "existing chat")
		assertHistory(rooms, ownerID, viewerID, 0, 10, "existing chat")
		_, err := rooms.ChatHistory(ctx, ownerID, otherID, 0, 10)
		check(err, live.ErrNotMember)
		_, err = rooms.ChatHistory(ctx, otherID, otherID, 0, 10)
		check(err, live.ErrRoomNotFound)
		_, err = rooms.ChatHistory(ctx, viewerID, viewerID, 0, 10)
		check(err, live.ErrRoomClosed)
		check(rooms.Leave(ctx, ownerID, viewerID), nil)
		_, err = rooms.ChatHistory(ctx, ownerID, viewerID, 0, 10)
		check(err, live.ErrNotMember)
		check(rooms.Join(ctx, ownerID, viewerID, ""), nil)
		sentID, err = rooms.SendChat(ctx, live.ChatMessage{RoomID: ownerID, UserID: viewerID, Text: "legacy chat", Payload: payload})
		check(err, nil)
		title := "Updated title"
		_, err = rooms.UpdateSettings(ctx, ownerID, &title, nil)
		check(err, nil)
		_, err = rooms.Start(ctx, ownerID, title, "")
		check(err, nil)
	}()

	platform := open()
	defer platform.Close()
	rooms := platform.Rooms
	latest := assertHistory(rooms, ownerID, viewerID, 0, 1, "legacy chat")[0]
	if latest.ID != sentID || latest.UserID != viewerID || latest.Payload != payload {
		t.Fatalf("persisted chat = %+v, want message ID %d and unmodified payload", latest, sentID)
	}
	previous := assertHistory(rooms, ownerID, viewerID, 1, 1, "existing chat")[0]
	if previous.Payload != "" {
		t.Fatalf("existing plain chat payload = %q", previous.Payload)
	}
	assertHistory(rooms, ownerID, ownerID, 2, 1)
	check(rooms.Kick(ctx, ownerID, ownerID, viewerID), nil)
	_, err = rooms.ChatHistory(ctx, ownerID, viewerID, 0, 10)
	check(err, live.ErrNotMember)
	check(rooms.Close(ctx, ownerID, ownerID), nil)
	_, err = rooms.ChatHistory(ctx, ownerID, ownerID, 0, 10)
	check(err, live.ErrRoomClosed)
	_, err = rooms.Start(ctx, viewerID, "Reopened old broadcast", "")
	check(err, nil)
	assertHistory(rooms, viewerID, viewerID, 0, 10)
	_, err = rooms.Start(ctx, ownerID, "Next broadcast", "")
	check(err, nil)
	assertHistory(rooms, ownerID, ownerID, 0, 10)
	_, err = rooms.ChatHistory(ctx, ownerID, viewerID, 0, 10)
	check(err, live.ErrNotMember)
	check(rooms.Join(ctx, ownerID, viewerID, ""), nil)
	for i := 1; i <= live.HistoryLimit+1; i++ {
		check(rooms.Send(ctx, ownerID, ownerID, fmt.Sprintf("message %d", i)), nil)
	}
	assertHistory(rooms, ownerID, viewerID, 0, 2, "message 101", "message 100")
	assertHistory(rooms, ownerID, viewerID, live.HistoryLimit-1, 1, "message 2")
	assertHistory(rooms, ownerID, viewerID, live.HistoryLimit, 1)
	_, err = db.Exec(ctx, "UPDATE room_messages SET created_at=now()-interval '25 hours' WHERE room_id=$1 AND id>$2", ownerID, sentID)
	check(err, nil)
	assertHistory(rooms, ownerID, viewerID, 0, 10)
	check(rooms.Send(ctx, ownerID, ownerID, "after idle"), nil)
	assertHistory(rooms, ownerID, viewerID, 0, 10, "after idle")
	var count int
	var archived []string
	check(db.QueryRow(ctx, "SELECT count(*), array_agg(text ORDER BY id) FILTER (WHERE id<=$1) FROM room_messages", sentID).Scan(&count, &archived), nil)
	if want := []string{"previous broadcast", "at last update", "existing chat", "closed broadcast chat", "legacy chat"}; !slices.Equal(archived, want) || count != len(want)+live.HistoryLimit+2 {
		t.Fatalf("archived chat = %v, total %d, want %v, total %d", archived, count, want, len(want)+live.HistoryLimit+2)
	}
}
