package app_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/livebot"
	"open_live_platform/internal/store/postgres"
)

func TestPostgresRoomBotsWelcomeGiftAndCloseAcrossReplicas(t *testing.T) {
	cfg, db := postgresTestDatabase(t)
	ctx := t.Context()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	api, err := app.New(ctx, cfg)
	check(err)
	t.Cleanup(api.Close)
	signUp := func(name string) auth.Session {
		t.Helper()
		session, err := api.Accounts.SignUp(ctx, name+"@bots.test", "password", name)
		check(err)
		return session
	}
	anchor, secondAnchor, robot, ordinary, holder := signUp("anchor"), signUp("second"), signUp("robot"), signUp("ordinary"), signUp("holder")
	_, err = db.Exec(ctx, "INSERT INTO nft_bindings(user_id,token_id,address) VALUES($1,21,$2)", holder.User.ID, "0x"+strings.Repeat("1", 40))
	check(err)

	var charges atomic.Int64
	billing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost || r.URL.Path != "/api/payment/point/decrease" || r.Header.Get("APPID") != "room-bot-test" {
			http.NotFound(w, r)
			return
		}
		var debit struct{ Point int64 }
		if err := json.NewDecoder(r.Body).Decode(&debit); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		call := charges.Add(1)
		if debit.Point == 201 {
			fmt.Fprint(w, `{"code":400,"reason":"POINT_NOT_ENOUGH"}`)
			return
		}
		fmt.Fprintf(w, `{"code":200,"data":{"order_id":"%d","order_no":"test-%d","total_point":%d}}`, call+100, call, debit.Point)
	}))
	t.Cleanup(billing.Close)
	cfg.GiftCatalogFile = filepath.Join(t.TempDir(), "gifts.json")
	check(os.WriteFile(cfg.GiftCatalogFile, []byte(`[
		{"id":"501","name":"Flower","price":200,"giftType":1},
		{"id":"502","name":"Rose","price":50,"giftType":1},
		{"id":"503","name":"Vase","price":201,"giftType":1},
		{"id":"504","name":"Candy","price":200,"giftType":2}
	]`), 0600))
	cfg.PaymentHost, cfg.PaymentAppID = billing.URL, "room-bot-test"
	cfg.RoomBots = livebot.Config{
		Accounts: []livebot.Account{{ID: robot.User.ID, Locale: "ja"}},
		Scripts: map[string]livebot.Script{"ja": {
			Welcome: []string{"ようこそ"},
			Gifts:   map[int8][]string{1: {"プレゼントありがとう"}},
		}},
		RoomPopulation: 20, StaySeconds: 60, WelcomeDelaySeconds: 30, GiftDelaySeconds: 30, MinimumGiftPoints: 100,
	}
	open := func() (*app.App, *postgres.Store) {
		t.Helper()
		platform, err := app.New(ctx, cfg)
		check(err)
		t.Cleanup(platform.Close)
		return platform, platform.Rooms.(*postgres.Store)
	}
	first, a := open()
	second, b := open()
	twoWorkers := func() {
		t.Helper()
		done := make(chan error, 2)
		go func() { done <- a.SweepRoomBots(ctx) }()
		go func() { done <- b.SweepRoomBots(ctx) }()
		check(<-done)
		check(<-done)
	}
	room, err := a.Start(ctx, anchor.User.ID, "Open", "")
	check(err)
	other, err := b.Start(ctx, secondAnchor.User.ID, "Other", "private")
	check(err)
	events, unsub, err := b.Subscribe(ctx, room.ID, anchor.User.ID)
	check(err)
	defer unsub()
	expectEvent := func(kind string, sender int64, text string) {
		t.Helper()
		deadline := time.After(4 * time.Second)
		for {
			select {
			case event, ok := <-events:
				if !ok {
					t.Fatal("room event stream closed before", kind)
				}
				if event.Type == kind && event.UserID == sender && event.Text == text {
					return
				}
			case <-deadline:
				t.Fatalf("event %s from %d, text %q not received", kind, sender, text)
			}
		}
	}
	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		check(db.QueryRow(ctx, query, args...).Scan(&n))
		return n
	}
	expectCounts := func(assignments, replies, chats int) {
		t.Helper()
		bots := count("SELECT count(*) FROM room_bot_members")
		pending := count("SELECT count(*) FROM room_bot_replies")
		sent := count("SELECT count(*) FROM room_messages WHERE user_id=$1", robot.User.ID)
		if bots != assignments || pending != replies || sent != chats {
			t.Fatalf("got %d bot placements, %d pending replies and %d sent chats, want %d/%d/%d",
				bots, pending, sent, assignments, replies, chats)
		}
	}
	twoWorkers()
	expectEvent("join", robot.User.ID, "")
	expectCounts(1, 0, 0)
	if count("SELECT count(*) FROM room_members WHERE room_id=$1", other.ID) != 0 {
		t.Fatal("bot joined a password-protected room")
	}
	check(a.Join(ctx, room.ID, ordinary.User.ID, ""))
	expectCounts(1, 0, 0)
	check(a.Join(ctx, room.ID, holder.User.ID, ""))
	check(b.Join(ctx, room.ID, holder.User.ID, ""))
	expectCounts(1, 1, 0)

	send := func(platform *app.App, giftID int, key, status string) {
		t.Helper()
		payload := fmt.Sprintf(`{"date_room_id":"%d","live_id":"%d","gift_id":"%d","limit_id":"0","sender_id":"%d","receiver_id":"%d","quantity":1}`,
			room.ID, room.Session.LiveID, giftID, ordinary.User.ID, anchor.User.ID)
		r := httptest.NewRequest(http.MethodPost, "/api/date/1vN/give/gifts", strings.NewReader(payload)).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+ordinary.Token)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		platform.Server.Handler.ServeHTTP(w, r)
		var response struct{ Status string }
		check(json.NewDecoder(w.Body).Decode(&response))
		if w.Code != http.StatusOK || response.Status != status {
			t.Fatalf("gift %d = %d %+v", giftID, w.Code, response)
		}
	}
	send(first, 501, "paid", "200")
	send(second, 501, "paid", "200")
	send(second, 502, "below", "200")
	send(first, 503, "declined", "4046")
	send(first, 504, "wrong-type", "200")
	if charges.Load() != 4 {
		t.Fatalf("payment API calls = %d, want 4", charges.Load())
	}
	expectCounts(1, 2, 0)
	twoWorkers()
	expectCounts(1, 2, 0)
	_, err = db.Exec(ctx, "UPDATE room_bot_replies SET send_at=now()-interval '1 second'")
	check(err)
	// A replacement instance can send the jobs queued by either existing instance.
	_, restarted := open()
	b = restarted
	twoWorkers()
	expectEvent("chat", robot.User.ID, "ようこそ")
	expectEvent("chat", robot.User.ID, "プレゼントありがとう")
	expectCounts(1, 0, 2)
	twoWorkers()
	expectCounts(1, 0, 2)

	send(first, 501, "pending-close", "200")
	expectCounts(1, 1, 2)
	check(b.Close(ctx, room.ID, anchor.User.ID))
	check(a.Close(ctx, room.ID, anchor.User.ID))
	expectEvent("close", anchor.User.ID, "")
	expectCounts(0, 0, 2)
	restartedRoom, err := a.Start(ctx, room.ID, "Another live", "")
	check(err)
	if restartedRoom.Session.LiveID == room.Session.LiveID {
		t.Fatal("broadcast identity was reused")
	}
	twoWorkers()
	expectCounts(1, 0, 2)
	_, err = db.Exec(ctx, "UPDATE room_bot_members SET expires_at=now()-interval '1 second'")
	check(err)
	twoWorkers()
	// With two workers the second can immediately place the freed bot again.
	if count("SELECT count(*) FROM room_bot_members WHERE expires_at<=now()") != 0 {
		t.Fatal("expired bot placement remained active")
	}
}
