package live

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"
)

func TestRoomLifecycle(t *testing.T) {
	store := NewStore()
	const owner, viewer = 1, 2
	room, err := store.Start(t.Context(), owner, "Live", "")
	if err != nil || !room.Online || room.IsLock || room.MemberCount != 0 {
		t.Fatalf("Start = %+v, %v", room, err)
	}
	ownerEvents, cancel, err := store.Subscribe(t.Context(), room.ID, owner)
	requireError(t, err, nil)
	defer cancel()

	requireError(t, store.Join(t.Context(), room.ID, viewer, ""), nil)
	requireError(t, store.Join(t.Context(), room.ID, viewer, "unneeded"), nil)
	requireEvent(t, ownerEvents, Event{Type: "join", RoomID: room.ID, UserID: viewer})
	members, err := store.Members(t.Context(), room.ID)
	if err != nil || !slices.Equal(members, []int64{viewer}) {
		t.Fatalf("Members = %v, %v", members, err)
	}

	viewerEvents, cancelViewer, err := store.Subscribe(t.Context(), room.ID, viewer)
	requireError(t, err, nil)
	defer cancelViewer()
	requireError(t, store.Leave(t.Context(), room.ID, viewer), nil)
	left := Event{Type: "leave", RoomID: room.ID, UserID: viewer}
	requireEvent(t, ownerEvents, left)
	requireEvent(t, viewerEvents, left)
	requireClosed(t, viewerEvents)

	requireError(t, store.Close(t.Context(), room.ID, viewer), ErrForbidden)
	requireError(t, store.Leave(t.Context(), room.ID, owner), nil)
	requireEvent(t, ownerEvents, Event{Type: "close", RoomID: room.ID, UserID: owner})
	requireClosed(t, ownerEvents)
	info, err := store.Info(t.Context(), room.ID)
	if err != nil || info.Online || info.MemberCount != 0 {
		t.Fatalf("Info after close = %+v, %v", info, err)
	}
	requireError(t, store.Join(t.Context(), room.ID, viewer, ""), ErrRoomClosed)
	newRoom, err := store.Start(t.Context(), owner, "Live again", "")
	if err != nil || newRoom.ID != room.ID || !newRoom.Online || newRoom.Title != "Live again" {
		t.Fatalf("Restart = %+v, %v", newRoom, err)
	}
}

func TestModerationAndChat(t *testing.T) {
	store := NewStore()
	const owner, admin, viewer = 1, 2, 3
	room, err := store.Start(t.Context(), owner, "Live", "")
	requireError(t, err, nil)
	requireError(t, store.SetAdmin(t.Context(), room.ID, admin, viewer, true), ErrForbidden)
	requireError(t, store.SetAdmin(t.Context(), room.ID, owner, admin, true), nil)
	admins, err := store.Admins(t.Context(), room.ID)
	if err != nil || !slices.Equal(admins, []int64{admin}) {
		t.Fatalf("Admins = %v, %v", admins, err)
	}
	requireError(t, store.Send(t.Context(), room.ID, viewer, "hello"), ErrNotMember)
	requireError(t, store.Join(t.Context(), room.ID, viewer, ""), nil)
	viewerEvents, cancelViewer, err := store.Subscribe(t.Context(), room.ID, viewer)
	requireError(t, err, nil)
	defer cancelViewer()
	ownerEvents, cancelOwner, err := store.Subscribe(t.Context(), room.ID, owner)
	requireError(t, err, nil)
	defer cancelOwner()

	requireError(t, store.Mute(t.Context(), room.ID, admin, owner, time.Minute), ErrForbidden)
	requireError(t, store.Mute(t.Context(), room.ID, admin, viewer, time.Minute), nil)
	requireError(t, store.Send(t.Context(), room.ID, viewer, "hello"), ErrMuted)
	requireError(t, store.Mute(t.Context(), room.ID, admin, viewer, 0), nil)
	requireError(t, store.Send(t.Context(), room.ID, viewer, "hello"), nil)
	chat := Event{Type: "chat", RoomID: room.ID, UserID: viewer, Text: "hello"}
	requireChatIdentity(t, store, &chat)
	requireEvent(t, viewerEvents, chat)
	requireEvent(t, ownerEvents, chat)

	requireError(t, store.Kick(t.Context(), room.ID, admin, owner), ErrForbidden)
	requireError(t, store.Kick(t.Context(), room.ID, admin, viewer), nil)
	kicked := Event{Type: "kick", RoomID: room.ID, UserID: admin, TargetID: viewer}
	requireEvent(t, viewerEvents, kicked)
	requireEvent(t, ownerEvents, kicked)
	requireClosed(t, viewerEvents)
	requireError(t, store.Join(t.Context(), room.ID, viewer, ""), ErrKicked)
	requireError(t, store.SetAdmin(t.Context(), room.ID, owner, admin, false), nil)
	requireError(t, store.Mute(t.Context(), room.ID, admin, viewer, time.Minute), ErrForbidden)

	requireError(t, store.Close(t.Context(), room.ID, owner), nil)
	_, err = store.Start(t.Context(), owner, "New broadcast", "")
	requireError(t, err, nil)
	requireError(t, store.Join(t.Context(), room.ID, viewer, ""), nil)
}

func TestRoomPasswordAttempts(t *testing.T) {
	store := NewStore()
	ctx := t.Context()
	const owner, viewer, otherViewer, otherOwner = 1, 2, 3, 4
	const password = "doors open"
	room, err := store.Start(ctx, owner, "Live", password)
	requireError(t, err, nil)
	if !room.IsLock {
		t.Fatalf("Start = %+v, want locked room", room)
	}
	events, cancel, err := store.Subscribe(ctx, room.ID, owner)
	requireError(t, err, nil)
	defer cancel()
	requireError(t, store.Join(ctx, room.ID, owner, "wrong"), nil)

	for range MaxRoomPasswordAttempts - 1 {
		requireError(t, store.Join(ctx, room.ID, viewer, ""), ErrInvalidRoomPassword)
	}
	_, _, err = store.Subscribe(ctx, room.ID, viewer)
	requireError(t, err, ErrNotMember)
	requireError(t, store.Join(ctx, room.ID, viewer, password), nil)
	requireEvent(t, events, Event{Type: "join", RoomID: room.ID, UserID: viewer})
	requireError(t, store.Join(ctx, room.ID, viewer, password), nil)
	select {
	case got := <-events:
		t.Fatalf("duplicate join event: %+v", got)
	default:
	}

	// A successful join resets the counter and an existing member must still authenticate.
	for range MaxRoomPasswordAttempts {
		requireError(t, store.Join(ctx, room.ID, viewer, ""), ErrInvalidRoomPassword)
	}
	requireError(t, store.Join(ctx, room.ID, viewer, password), ErrRoomPasswordLocked)
	requireError(t, store.Join(ctx, room.ID, viewer, ""), ErrRoomPasswordLocked)
	requireError(t, store.Leave(ctx, room.ID, viewer), nil)
	requireError(t, store.Join(ctx, room.ID, viewer, password), ErrRoomPasswordLocked)
	requireError(t, store.Join(ctx, room.ID, otherViewer, password), nil)
	otherRoom, err := store.Start(ctx, otherOwner, "Another live", password)
	requireError(t, err, nil)
	requireError(t, store.Join(ctx, otherRoom.ID, viewer, password), nil)

	requireError(t, store.Close(ctx, room.ID, owner), nil)
	closed, err := store.Info(ctx, room.ID)
	requireError(t, err, nil)
	if closed.Online || closed.IsLock {
		t.Fatalf("Info after close = %+v, want offline and unlocked", closed)
	}
	requireError(t, store.Join(ctx, room.ID, viewer, password), ErrRoomClosed)
	const newPassword = "new password"
	reopened, err := store.Start(ctx, owner, "New live", newPassword)
	requireError(t, err, nil)
	if reopened.ID != room.ID || !reopened.IsLock {
		t.Fatalf("Restart = %+v, want the same room ID and locked", reopened)
	}
	requireError(t, store.Join(ctx, room.ID, viewer, password), ErrInvalidRoomPassword)
	requireError(t, store.Join(ctx, room.ID, viewer, newPassword), nil)
}

func TestStartPreservesOnlineRoomPassword(t *testing.T) {
	store := NewStore()
	ctx := t.Context()
	const owner = 1
	public, err := store.Start(ctx, owner, "Public", "")
	requireError(t, err, nil)
	_, err = store.Start(ctx, owner, "Changed title", "new password")
	requireError(t, err, ErrRoomAlreadyOnline)
	if got, err := store.Info(ctx, owner); err != nil || got != public {
		t.Fatalf("Info = %+v, %v, want %+v", got, err, public)
	}
	// Owner departure also ends the current broadcast.
	requireError(t, store.Leave(ctx, owner, owner), nil)
	const password = "secret"
	locked, err := store.Start(ctx, owner, "Private", password)
	requireError(t, err, nil)
	for _, repeatPassword := range []string{"", password} {
		got, err := store.Start(ctx, owner, "Changed title", repeatPassword)
		if err != nil || got != locked {
			t.Fatalf("Repeat start = %+v, %v, want %+v", got, err, locked)
		}
	}
	_, err = store.Start(ctx, owner, "Changed title", "different password")
	requireError(t, err, ErrRoomAlreadyOnline)
	rooms, err := store.Rooms(ctx)
	if err != nil || !slices.Equal(rooms, []Room{locked}) {
		t.Fatalf("Rooms = %+v, %v, want %+v", rooms, err, []Room{locked})
	}
	response, err := json.Marshal(rooms[0])
	requireError(t, err, nil)
	var fields map[string]json.RawMessage
	requireError(t, json.Unmarshal(response, &fields), nil)
	if string(fields["isLock"]) != "true" || len(fields) != 6 {
		t.Fatalf("room response = %s, want public fields with isLock=true", response)
	}
}

func TestConcurrentRoomPasswordAttempts(t *testing.T) {
	store := NewStore()
	ctx := t.Context()
	const owner, viewer = 1, 2
	const password = "secret"
	room, err := store.Start(ctx, owner, "Live", password)
	requireError(t, err, nil)
	for range MaxRoomPasswordAttempts - 1 {
		requireError(t, store.Join(ctx, room.ID, viewer, ""), ErrInvalidRoomPassword)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			results <- store.Join(ctx, room.ID, viewer, "wrong")
		}()
	}
	close(start)
	resultCounts := make(map[error]int)
	for range 2 {
		resultCounts[<-results]++
	}
	if resultCounts[ErrInvalidRoomPassword] != 1 || resultCounts[ErrRoomPasswordLocked] != 1 {
		t.Fatalf("concurrent results = %v, want one incorrect password and one lockout", resultCounts)
	}
	requireError(t, store.Join(ctx, room.ID, viewer, password), ErrRoomPasswordLocked)
}

func TestUpdateSettingsKeepsBroadcastAndSubscriptions(t *testing.T) {
	store := NewStore()
	ctx := t.Context()
	const owner, viewer, newViewer = 1, 2, 3
	const oldPassword = "old password"
	room, err := store.Start(ctx, owner, "Live", oldPassword)
	requireError(t, err, nil)
	requireError(t, store.Join(ctx, room.ID, viewer, oldPassword), nil)
	requireError(t, store.SetAdmin(ctx, room.ID, owner, viewer, true), nil)
	events, cancel, err := store.Subscribe(ctx, room.ID, viewer)
	requireError(t, err, nil)
	defer cancel()

	emptyTitle := ""
	updated, err := store.UpdateSettings(ctx, owner, RoomSettings{Title: &emptyTitle})
	requireError(t, err, nil)
	if updated.Title != "" || !updated.IsLock {
		t.Fatalf("title update = %+v, want an empty title and a locked room", updated)
	}
	requireError(t, store.Join(ctx, room.ID, viewer, oldPassword), nil)

	newTitle, newPassword := "  Live again  ", " new password "
	updated, err = store.UpdateSettings(ctx, owner, RoomSettings{Title: &newTitle, Password: &newPassword})
	requireError(t, err, nil)
	want := room
	want.Title, want.MemberCount = newTitle, 1
	if updated != want {
		t.Fatalf("password update = %+v, want %+v", updated, want)
	}
	requireError(t, store.Join(ctx, room.ID, owner, ""), nil)
	requireError(t, store.Send(ctx, room.ID, viewer, "Still watching"), nil)
	chat := Event{Type: "chat", RoomID: room.ID, UserID: viewer, Text: "Still watching"}
	requireChatIdentity(t, store, &chat)
	requireEvent(t, events, chat)
	admins, err := store.Admins(ctx, room.ID)
	if err != nil || !slices.Equal(admins, []int64{viewer}) {
		t.Fatalf("Admins = %v, %v, want [%d]", admins, err, viewer)
	}
	requireError(t, store.Join(ctx, room.ID, newViewer, oldPassword), ErrInvalidRoomPassword)
	requireError(t, store.Join(ctx, room.ID, newViewer, newPassword), nil)
	_, err = store.UpdateSettings(ctx, viewer, RoomSettings{Title: &emptyTitle})
	requireError(t, err, ErrRoomNotFound)
	requireError(t, store.Close(ctx, room.ID, owner), nil)
	_, err = store.UpdateSettings(ctx, owner, RoomSettings{Password: &newPassword})
	requireError(t, err, ErrRoomClosed)
}

func TestUpdateSettingsClearsAttemptsOnlyWhenPublic(t *testing.T) {
	store := NewStore()
	ctx := t.Context()
	const owner, firstViewer, secondViewer = 1, 2, 3
	room, err := store.Start(ctx, owner, "Live", "old password")
	requireError(t, err, nil)
	for range MaxRoomPasswordAttempts {
		requireError(t, store.Join(ctx, room.ID, firstViewer, ""), ErrInvalidRoomPassword)
		requireError(t, store.Join(ctx, room.ID, secondViewer, ""), ErrInvalidRoomPassword)
	}

	newPassword := "new password"
	updated, err := store.UpdateSettings(ctx, owner, RoomSettings{Password: &newPassword})
	requireError(t, err, nil)
	if updated != room {
		t.Fatalf("password update = %+v, want %+v", updated, room)
	}
	requireError(t, store.Join(ctx, room.ID, firstViewer, newPassword), ErrRoomPasswordLocked)
	requireError(t, store.Join(ctx, room.ID, secondViewer, newPassword), ErrRoomPasswordLocked)

	public := ""
	updated, err = store.UpdateSettings(ctx, owner, RoomSettings{Password: &public})
	requireError(t, err, nil)
	if updated.IsLock {
		t.Fatalf("public update = %+v, want an unlocked room", updated)
	}
	_, err = store.UpdateSettings(ctx, owner, RoomSettings{Password: &newPassword})
	requireError(t, err, nil)
	requireError(t, store.Join(ctx, room.ID, firstViewer, newPassword), nil)
	requireError(t, store.Join(ctx, room.ID, secondViewer, newPassword), nil)
}

func TestJoinRechecksPasswordAfterVerification(t *testing.T) {
	store := NewStore()
	ctx := t.Context()
	const owner, viewer = 1, 2
	const oldPassword, newPassword = "old password", "new password"
	room, err := store.Start(ctx, owner, "Live", oldPassword)
	requireError(t, err, nil)
	newHash, err := HashRoomPassword(newPassword)
	requireError(t, err, nil)

	checks := 0
	joinCtx := joinCheckContext{Context: ctx, check: func() {
		checks++
		if checks == 2 {
			// Apply the change at the point where Join has reacquired the store lock.
			store.rooms[room.ID].passwordHash = newHash
		}
	}}
	requireError(t, store.Join(joinCtx, room.ID, viewer, oldPassword), ErrInvalidRoomPassword)
	requireError(t, store.Join(ctx, room.ID, viewer, newPassword), nil)
}

type joinCheckContext struct {
	context.Context
	check func()
}

func (ctx joinCheckContext) Err() error {
	ctx.check()
	return ctx.Context.Err()
}

func requireError(t *testing.T, got, want error) {
	t.Helper()
	if got != want {
		t.Fatalf("error = %v, want %v", got, want)
	}
}

func requireChatIdentity(t *testing.T, store *Store, want *Event) {
	t.Helper()
	messages, err := store.ChatHistory(t.Context(), want.RoomID, want.UserID, 0, 1)
	if err != nil || len(messages) != 1 || messages[0].ID < 1 || messages[0].CreatedAt.IsZero() {
		t.Fatalf("chat history = %+v, %v, want the stored message identity", messages, err)
	}
	want.MessageID, want.CreatedAt = messages[0].ID, messages[0].CreatedAt
}

func requireEvent(t *testing.T, events <-chan Event, want Event) {
	t.Helper()
	select {
	case got, ok := <-events:
		if !ok || got != want {
			t.Fatalf("event = %+v, open = %v, want %+v", got, ok, want)
		}
	default:
		t.Fatalf("event %+v was not delivered", want)
	}
}

func requireClosed(t *testing.T, events <-chan Event) {
	t.Helper()
	select {
	case got, ok := <-events:
		if ok {
			t.Fatalf("received %+v, want closed subscription", got)
		}
	default:
		t.Fatal("subscription is still open")
	}
}
