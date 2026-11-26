package app_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/live"
	"open_live_platform/internal/livebot"
	"open_live_platform/internal/store/postgres"
)

func TestPostgresRoomBotPoolLikesDailyAndH5AcrossReplicas(t *testing.T) {
	config, db := postgresTestDatabase(t)
	ctx := t.Context()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	baseline, err := app.New(ctx, config)
	check(err)
	t.Cleanup(baseline.Close)
	signup := func(name string) auth.Session {
		t.Helper()
		res, err := baseline.Accounts.SignUp(ctx, name+"@dailybot.test", "room-bot-test", name)
		check(err)
		return res
	}
	hostA, hostB, private := signup("host-a"), signup("host-b"), signup("host-c")
	bot, configuredOnly := signup("robot"), signup("configured-only")
	seed := baseline.Rooms.(*postgres.Store)
	entries := []livebot.LegacyPoolEntry{
		{UserID: bot.User.ID, State: "idle"},
		{UserID: bot.User.ID, State: "inuse", AllocatedAt: 12345678},
		{UserID: 9999999, State: "idle"},
	}
	want := postgres.LegacyRoomBotPoolResult{Imported: 1, Missing: 1, Idle: 1, InUse: 1}
	got, err := seed.ImportLegacyRoomBotPool(ctx, entries)
	if err != nil || got != want {
		t.Fatalf("first pool import = %+v, %v", got, err)
	}
	want.Imported, want.Existing = 0, 1
	got, err = seed.ImportLegacyRoomBotPool(ctx, entries)
	if err != nil || got != want {
		t.Fatalf("repeat pool import = %+v, %v", got, err)
	}
	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		check(db.QueryRow(ctx, query, args...).Scan(&n))
		return n
	}
	if count("SELECT count(*) FROM room_bot_members") != 0 {
		t.Fatal("Redis in-use snapshot copied an old membership")
	}
	tables := map[string][]byte{
		"robot_create_property":       []byte(fmt.Sprintf(`[{"uid":%d,"location":"ja"},{"uid":%d,"location":"en"}]`, bot.User.ID, configuredOnly.User.ID)),
		"robot_welcome_message_text":  []byte("[]"),
		"robot_interactive_gift_text": []byte("[]"),
		"robot_like_time":             []byte(`[{"time":11,"weight":5}]`),
		"robot_daily_time":            []byte(`[{"time":42,"weight":8}]`),
		"robot_daily_text":            []byte(`[{"weight":"JA","text":"こんばんは"},{"weight":"en","text":"English only"}]`),
	}
	config.RoomBots, err = livebot.FromLuban(livebot.Config{DatabasePool: true, DailyChat: true, RoomPopulation: 20, StaySeconds: 120}, tables)
	check(err)
	open := func() (*app.App, *postgres.Store) {
		t.Helper()
		p, err := app.New(ctx, config)
		check(err)
		t.Cleanup(p.Close)
		return p, p.Rooms.(*postgres.Store)
	}
	_, a := open()
	peer, b := open()
	work := func() {
		t.Helper()
		done := make(chan error, 2)
		go func() { done <- a.SweepRoomBots(ctx) }()
		go func() { done <- b.SweepRoomBots(ctx) }()
		check(<-done)
		check(<-done)
	}
	roomA, err := a.Start(ctx, hostA.User.ID, "open A", "")
	check(err)
	roomB, err := b.Start(ctx, hostB.User.ID, "open B", "")
	check(err)
	privateRoom, err := a.Start(ctx, private.User.ID, "private", "secret")
	check(err)
	work()
	if count("SELECT count(*) FROM room_bot_members") != 1 || count("SELECT count(*) FROM room_bot_members WHERE room_id=$1 OR user_id=$2", privateRoom.ID, configuredOnly.User.ID) != 0 {
		t.Fatal("incorrect number of exclusive bot leases or an unauthorized bot joined")
	}
	var roomID, leaseUser int64
	var likeSecs, dailySecs int
	check(db.QueryRow(ctx, `SELECT room_id,user_id,round(extract(epoch FROM(like_at-(expires_at-interval '120 seconds')))),
		round(extract(epoch FROM(daily_at-(expires_at-interval '120 seconds')))) FROM room_bot_members`).
		Scan(&roomID, &leaseUser, &likeSecs, &dailySecs))
	if leaseUser != bot.User.ID || likeSecs != 11 || dailySecs != 5 {
		t.Fatalf("lease account/time = %d, %d, %d", leaseUser, likeSecs, dailySecs)
	}
	room, host, other := roomA, hostA, roomB
	if roomID == roomB.ID {
		room, host, other = roomB, hostB, roomA
	}
	socket := botRoomSocket(t, peer, host, room)
	expectFrame := func(kind string) json.RawMessage {
		t.Helper()
		for i := 0; i < 12; i++ {
			var frame struct {
				Content string
				Payload []struct {
					Sender  string
					Message struct {
						Kind string          `json:"msgType"`
						Data json.RawMessage `json:"message"`
					}
				}
			}
			check(socket.ReadJSON(&frame))
			if frame.Content == "live_chat" && len(frame.Payload) == 1 && frame.Payload[0].Message.Kind == kind {
				item := frame.Payload[0]
				if item.Sender != strconv.FormatInt(bot.User.ID, 10) {
					t.Fatalf("bot relay sender %q", item.Sender)
				}
				return item.Message.Data
			}
		}
		t.Fatalf("bot relay event %q missing", kind)
		return nil
	}
	_, err = db.Exec(ctx, "UPDATE room_bot_members SET like_at=now()-interval '2 seconds',daily_at=now()-interval '1 second' WHERE room_id=$1", roomID)
	check(err)
	work()
	work()
	var like struct {
		Room  string `json:"roomId"`
		Live  string `json:"liveId"`
		User  string `json:"thumbPeople"`
		Count int64  `json:"curentLikeCount"`
	}
	check(json.Unmarshal(expectFrame("Z"), &like))
	if like.Count != 1 || like.Room != strconv.FormatInt(roomID, 10) ||
		like.Live != strconv.FormatInt(room.Session.LiveID, 10) || like.User != strconv.FormatInt(bot.User.ID, 10) {
		t.Fatalf("H5 room like payload %+v", like)
	}
	var text string
	check(json.Unmarshal(expectFrame("C"), &text))
	if text != "こんばんは" {
		t.Fatalf("Luban local-language daily chat = %q", text)
	}
	if count("SELECT like_total FROM room_broadcast_metrics WHERE live_id=$1", room.Session.LiveID) != 1 ||
		count("SELECT likes FROM room_broadcast_viewer_likes WHERE live_id=$1 AND user_id=$2", room.Session.LiveID, bot.User.ID) != 1 ||
		count("SELECT count(*) FROM room_messages WHERE room_id=$1 AND user_id=$2", roomID, bot.User.ID) != 1 {
		t.Fatal("concurrent sweeps duplicated a like or chat")
	}
	var nextDaily int
	check(db.QueryRow(ctx, `SELECT round(extract(epoch FROM daily_at-now())) FROM room_bot_members WHERE room_id=$1`, roomID).Scan(&nextDaily))
	if nextDaily < 40 || nextDaily > 43 {
		t.Fatalf("Luban daily recurrence = %d", nextDaily)
	}

	check(a.Mute(ctx, roomID, host.User.ID, bot.User.ID, time.Minute))
	_, err = db.Exec(ctx, "UPDATE room_bot_members SET daily_at=now()-interval '1 second' WHERE room_id=$1", roomID)
	check(err)
	work()
	if count("SELECT count(*) FROM room_messages WHERE room_id=$1", roomID) != 1 {
		t.Fatal("bot ignored mute")
	}
	check(a.Close(ctx, roomID, host.User.ID))
	work()
	if count("SELECT count(*) FROM room_bot_members WHERE room_id=$1 AND user_id=$2", other.ID, bot.User.ID) != 1 {
		t.Fatal("closing broadcast did not release the shared account")
	}
	_, err = db.Exec(ctx, "UPDATE room_bot_members SET daily_at=now()-interval '1 second' WHERE room_id=$1", other.ID)
	check(err)
	config.RoomBots.DailyChat = false
	check(a.ConfigureRoomBots(config.RoomBots))
	check(b.ConfigureRoomBots(config.RoomBots))
	work()
	if count("SELECT count(*) FROM room_messages WHERE room_id=$1", other.ID) != 0 {
		t.Fatal("disabled daily chat was delivered")
	}
	_, err = db.Exec(ctx, `UPDATE room_bot_members SET like_at=now()-interval '2 seconds',
		expires_at=now()-interval '1 second' WHERE room_id=$1`, other.ID)
	check(err)
	check(a.SweepRoomBots(ctx))
	if count("SELECT like_total FROM room_broadcast_metrics WHERE live_id=$1", other.Session.LiveID) != 0 {
		t.Fatal("an expired lease voted")
	}
}

func botRoomSocket(t *testing.T, a *app.App, host auth.Session, room live.Room) *websocket.Conn {
	t.Helper()
	server := httptest.NewServer(a.Server.Handler)
	t.Cleanup(server.Close)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/api/device/qrcode", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+host.Token)
	res, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var ticket struct{ URL string }
	if err := json.NewDecoder(res.Body).Decode(&ticket); err != nil {
		t.Fatal(err)
	}
	dialer := websocket.Dialer{Subprotocols: []string{"h5relayer"}, HandshakeTimeout: 5 * time.Second}
	socket, _, err := dialer.DialContext(t.Context(), ticket.URL+"?room_id="+strconv.FormatInt(room.ID, 10), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { socket.Close() })
	if err := socket.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var greeting json.RawMessage
	if err := socket.ReadJSON(&greeting); err != nil {
		t.Fatal(err)
	}
	return socket
}
