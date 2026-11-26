package app_test

import (
	"errors"
	"testing"
	"time"

	"open_live_platform/internal/app"
	"open_live_platform/internal/live"
)

func TestPostgresRoomCommandsStayInSubscribedBroadcast(t *testing.T) {
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
	socket, broadcaster := start(), start()
	owner, err := broadcaster.Accounts.SignUp(ctx, "owner@example.org", "secret", "Owner")
	check(err)
	viewer, err := broadcaster.Accounts.SignUp(ctx, "viewer@example.org", "secret", "Viewer")
	check(err)
	roomID, viewerID := owner.User.ID, viewer.User.ID
	first, err := broadcaster.Rooms.Start(ctx, roomID, "First broadcast", "")
	check(err)
	oldSub, err := socket.Rooms.SubscribeLive(ctx, roomID, roomID)
	check(err)
	t.Cleanup(oldSub.Unsubscribe)
	if oldSub.LiveID != first.Session.LiveID {
		t.Fatalf("subscription LiveID = %d, want %d", oldSub.LiveID, first.Session.LiveID)
	}
	oldCommands := live.WithRoomCommandScope(ctx, roomID, oldSub.LiveID)

	check(broadcaster.Rooms.Close(ctx, roomID, roomID))
	second, err := broadcaster.Rooms.Start(ctx, roomID, "Second broadcast", "")
	check(err)
	check(broadcaster.Rooms.Join(ctx, roomID, viewerID, ""))
	expectClosed := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, live.ErrRoomClosed) {
			t.Fatalf("stale %s error = %v, want %v", name, err, live.ErrRoomClosed)
		}
	}
	expectClosed("chat", socket.Rooms.Send(oldCommands, roomID, roomID, "First broadcast chat"))
	expectClosed("mute", socket.Rooms.Mute(oldCommands, roomID, roomID, viewerID, time.Minute))
	expectClosed("unmute", socket.Rooms.Mute(oldCommands, roomID, roomID, viewerID, 0))
	expectClosed("kick", socket.Rooms.Kick(oldCommands, roomID, roomID, viewerID))
	expectClosed("close", socket.Rooms.Close(oldCommands, roomID, roomID))

	newSub, err := socket.Rooms.SubscribeLive(ctx, roomID, viewerID)
	check(err)
	t.Cleanup(newSub.Unsubscribe)
	if newSub.LiveID == oldSub.LiveID || newSub.LiveID != second.Session.LiveID {
		t.Fatalf("new subscription LiveID = %d, want %d", newSub.LiveID, second.Session.LiveID)
	}
	newCommands := live.WithRoomCommandScope(ctx, roomID, newSub.LiveID)
	const text = "Current broadcast chat"
	check(socket.Rooms.Send(newCommands, roomID, viewerID, text))
	history, err := broadcaster.Rooms.ChatHistory(ctx, roomID, roomID, 0, 10)
	check(err)
	if len(history) != 1 || history[0].Text != text {
		t.Fatalf("chat history = %+v, want the current broadcast message", history)
	}
}
