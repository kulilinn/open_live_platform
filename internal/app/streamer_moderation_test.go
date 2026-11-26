package app_test

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"open_live_platform/internal/app"
	"open_live_platform/internal/notice"
	"open_live_platform/internal/relay"
)

func TestPostgresRoomModerationNotifiesRemoteHost(t *testing.T) {
	a := startEditorialAPI(t, func(cfg *app.Config) {
		cfg.RelayRust = relay.RustOptions{AppKey: "moderation-fixture", AppSecret: "local-secret"}
	})
	ctx := t.Context()
	host, err := a.platform.Accounts.SignUp(ctx, "host@moderation.test", "password", "Host")
	if err != nil {
		t.Fatal(err)
	}
	other, err := a.platform.Accounts.SignUp(ctx, "other@moderation.test", "password", "Viewer")
	if err != nil {
		t.Fatal(err)
	}
	room, err := a.platform.Rooms.Start(ctx, host.User.ID, "original room", "")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(a.public)
	t.Cleanup(server.Close)
	target := openPrivateH5(t, server, host)
	t.Cleanup(func() { target.Close() })
	observer := openPrivateH5(t, server, other)
	t.Cleanup(func() { observer.Close() })
	id := strconv.FormatInt(host.User.ID, 10)
	// Exercise the persisted large-frame path as well as an ordinary PostgreSQL notification.
	for _, step := range []struct {
		action, content, reason string
		duration                int
	}{
		{"Alert", "streamer_alert", strings.Repeat("x", 8200), 0},
		{"EndStream", "streamer_end_stream", "end stream", 0},
		{"Disable", "streamer_disable", "disable stream", 60},
	} {
		a.call(t, "/api/operation/room/streamer_op", map[string]any{"ty": step.action, "streamerId": id, "roomId": id, "limitTime": step.duration, "reason": []string{step.reason}}, 200, nil)
		var msg struct {
			Content, Queue string
			Payload        struct {
				RoomID, UID   string
				Reason        []string
				LimitTotal    int
				LimitTime, TS int64
			}
		}
		for {
			if err := target.ReadJSON(&msg); err != nil {
				t.Fatal(err)
			}
			if msg.Content != "device" && msg.Content != "control" {
				break
			}
		}
		if msg.Content != step.content || msg.Queue != id || msg.Payload.RoomID != id || msg.Payload.UID != id ||
			msg.Payload.LimitTotal != step.duration || msg.Payload.TS == 0 || msg.Payload.LimitTime-msg.Payload.TS != int64(step.duration) ||
			len(msg.Payload.Reason) != 1 || msg.Payload.Reason[0] != step.reason {
			t.Fatalf("invalid %s moderation frame: content=%q queue=%q room=%q uid=%q duration=%d deadline=%d ts=%d reasons=%d", step.action, msg.Content, msg.Queue, msg.Payload.RoomID, msg.Payload.UID, msg.Payload.LimitTotal, msg.Payload.LimitTime, msg.Payload.TS, len(msg.Payload.Reason))
		}
	}
	if state, err := a.platform.Rooms.Info(ctx, room.ID); err != nil || state.Online || !state.Limited(time.Now()) {
		t.Fatalf("moderated room %+v, error=%v", state, err)
	}
	inbox, err := a.platform.Accounts.(notice.Store).Notices(ctx, host.User.ID, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[int8]int{}
	for _, item := range inbox.List {
		if item.Target == 1 && item.UserID == host.User.ID {
			counts[item.Type]++
		}
	}
	for _, ty := range []int8{11, 12, 13} {
		if counts[ty] != 1 {
			t.Fatalf("unexpected notices %v", counts)
		}
	}
	if err := observer.SetReadDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	for {
		var frame struct{ Content string }
		if err := observer.ReadJSON(&frame); err != nil {
			break
		}
		if strings.HasPrefix(frame.Content, "streamer_") {
			t.Fatal("another user received the moderation frame")
		}
	}
}
