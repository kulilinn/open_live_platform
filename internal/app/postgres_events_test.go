package app_test

import (
	"crypto/rand"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"open_live_platform/internal/app"
	"open_live_platform/internal/live"
)

func TestPostgresCrossInstanceRoomEvents(t *testing.T) {
	config, _ := postgresTestDatabase(t)
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
		t.Cleanup(platform.Close)
		return platform
	}
	first, second := start(), start()
	host, err := first.Accounts.SignUp(ctx, "host@example.org", "secret", "Host")
	check(err)
	firstViewer, err := first.Accounts.SignUp(ctx, "first@example.org", "secret", "First")
	check(err)
	secondViewer, err := first.Accounts.SignUp(ctx, "second@example.org", "secret", "Second")
	check(err)
	roomID := host.User.ID
	viewerID, otherID := firstViewer.User.ID, secondViewer.User.ID

	_, err = first.Rooms.Start(ctx, roomID, "Live", "")
	check(err)
	subscribe := func(platform *app.App, userID int64) <-chan live.Event {
		t.Helper()
		ch, stop, err := platform.Rooms.Subscribe(ctx, roomID, userID)
		check(err)
		t.Cleanup(stop)
		return ch
	}
	read := func(events <-chan live.Event, want live.Event, open bool) {
		t.Helper()
		select {
		case event, ok := <-events:
			if ok != open || !event.CreatedAt.Equal(want.CreatedAt) {
				t.Fatalf("event %q (stream open = %t) with time %v, want %q (stream open = %t) with time %v", event.Type, ok, event.CreatedAt, want.Type, open, want.CreatedAt)
			}
			event.CreatedAt = want.CreatedAt
			if event != want {
				t.Fatalf("event %q with message ID %d, want %q with message ID %d", event.Type, event.MessageID, want.Type, want.MessageID)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %q", want.Type)
		}
	}
	chatEvent := func(userID int64, text, payload string) live.Event {
		t.Helper()
		messages, err := first.Rooms.ChatHistory(ctx, roomID, userID, 0, 1)
		if err != nil || len(messages) != 1 || messages[0].ID < 1 || messages[0].CreatedAt.IsZero() {
			t.Fatalf("chat history = %+v, %v, want the stored message identity", messages, err)
		}
		return live.Event{
			Type: "chat", RoomID: roomID, UserID: userID, Text: text, Payload: payload,
			MessageID: messages[0].ID, CreatedAt: messages[0].CreatedAt,
		}
	}

	hostEvents := subscribe(second, roomID)
	check(first.Rooms.Join(ctx, roomID, viewerID, ""))
	read(hostEvents, live.Event{Type: "join", RoomID: roomID, UserID: viewerID}, true)
	viewerEvents := subscribe(second, viewerID)
	check(first.Rooms.Join(ctx, roomID, otherID, ""))
	join := live.Event{Type: "join", RoomID: roomID, UserID: otherID}
	read(hostEvents, join, true)
	read(viewerEvents, join, true)
	otherEvents := subscribe(second, otherID)

	text := strings.Repeat("聊天", 2048)
	payload := `{"message":"` + text + `"}`
	_, err = first.Rooms.SendChat(ctx, live.ChatMessage{RoomID: roomID, UserID: viewerID, Text: text, Payload: payload})
	check(err)
	chat := chatEvent(viewerID, text, payload)
	read(hostEvents, chat, true)
	read(viewerEvents, chat, true)
	read(otherEvents, chat, true)

	check(first.Rooms.Kick(ctx, roomID, roomID, viewerID))
	kick := live.Event{Type: "kick", RoomID: roomID, UserID: roomID, TargetID: viewerID}
	read(hostEvents, kick, true)
	read(viewerEvents, kick, true)
	read(viewerEvents, live.Event{}, false)
	read(otherEvents, kick, true)
	if _, _, err := second.Rooms.Subscribe(ctx, roomID, viewerID); !errors.Is(err, live.ErrNotMember) {
		t.Fatalf("kicked viewer subscription = %v", err)
	}

	check(first.Rooms.Leave(ctx, roomID, otherID))
	leave := live.Event{Type: "leave", RoomID: roomID, UserID: otherID}
	read(hostEvents, leave, true)
	read(otherEvents, leave, true)
	read(otherEvents, live.Event{}, false)

	check(first.Rooms.Close(ctx, roomID, roomID))
	closed, err := first.Rooms.Info(ctx, roomID)
	check(err)
	_, err = first.Rooms.Start(ctx, roomID, "Reopened", "")
	check(err)
	reopened := subscribe(second, roomID)
	read(hostEvents, live.Event{Type: "close", RoomID: roomID, UserID: roomID, CreatedAt: closed.Session.EndedAt}, true)
	read(hostEvents, live.Event{}, false)
	check(first.Rooms.Send(ctx, roomID, roomID, "new broadcast"))
	read(reopened, chatEvent(roomID, "new broadcast", ""), true)
}

func TestPostgresRoomEventsReconnect(t *testing.T) {
	config, db := postgresTestDatabase(t)
	ctx := t.Context()
	databaseURL, err := url.Parse(config.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	appName := "test_events_" + rand.Text()
	query := databaseURL.Query()
	query.Set("application_name", appName)
	databaseURL.RawQuery = query.Encode()
	config.DatabaseURL = databaseURL.String()
	platform, err := app.New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(platform.Close)

	owner, err := platform.Accounts.SignUp(ctx, "host@example.org", "secret", "Host")
	if err != nil {
		t.Fatal(err)
	}
	id := owner.User.ID
	if _, err := platform.Rooms.Start(ctx, id, "Live", ""); err != nil {
		t.Fatal(err)
	}
	events, stop, err := platform.Rooms.Subscribe(ctx, id, id)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	var terminated bool
	err = db.QueryRow(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE application_name=$1 AND query LIKE 'LISTEN open_live_rooms_%'`, appName).Scan(&terminated)
	if err != nil || !terminated {
		t.Fatalf("terminate test listener = %t, %v", terminated, err)
	}
	select {
	case _, open := <-events:
		if open {
			t.Fatal("old subscription received an unexpected event")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("old subscription did not close after losing the listener")
	}

	deadline := time.After(5 * time.Second)
	retry := time.NewTicker(20 * time.Millisecond)
	defer retry.Stop()
	for {
		events, stop, err = platform.Rooms.Subscribe(ctx, id, id)
		if err == nil {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("failed to resubscribe: %v", err)
		case <-retry.C:
		}
	}
	defer stop()
	if err := platform.Rooms.Send(ctx, id, id, "after reconnect"); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if event.Type != "chat" || event.Text != "after reconnect" {
			t.Fatalf("unexpected event after reconnect: %+v", event)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("chat was not delivered after listener reconnect")
	}
}
