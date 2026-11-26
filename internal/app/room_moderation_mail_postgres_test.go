package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/admin"
	"open_live_platform/internal/app"
	"open_live_platform/internal/notifyemail"
	"open_live_platform/internal/store/postgres"
)

func TestPostgresRoomModerationMail(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	type message struct {
		Template string              `json:"template_key"`
		From     notifyemail.Address `json:"from"`
		To       []struct {
			Address notifyemail.Address `json:"email_address"`
		} `json:"to"`
		Fields map[string]string `json:"merge_info"`
		Key    string            `json:"-"`
	}
	var calls atomic.Int64
	sent := make(chan message, 10)
	notification := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Type    int    `json:"type"`
			Content string `json:"content"`
		}
		var email message
		if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&body) != nil || body.Type != 3 ||
			json.Unmarshal([]byte(body.Content), &email) != nil {
			t.Error("message did not use the original notification wire format")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		email.Key = r.Header.Get("Idempotency-Key")
		sent <- email
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(notification.Close)
	from := notifyemail.Address{Address: "moderation@example.test", Name: "Moderators"}
	mailConfig := notifyemail.Config{URL: notification.URL, From: from}
	api := startEditorialAPI(t, func(cfg *app.Config) {
		cfg.Addr, cfg.GRPCAddr = "127.0.0.1:0", "127.0.0.1:0"
		cfg.RoomModerationMail = mailConfig
	})
	store, err := postgres.Open(ctx, api.platform.Config.DatabaseURL)
	check(err)
	t.Cleanup(store.Shutdown)
	replica, err := postgres.Open(ctx, api.platform.Config.DatabaseURL)
	check(err)
	t.Cleanup(replica.Shutdown)
	db, err := pgx.Connect(ctx, api.platform.Config.DatabaseURL)
	check(err)
	t.Cleanup(func() { db.Close(context.Background()) })
	sender, err := notifyemail.New(mailConfig)
	check(err)
	const email = "moderated-anchor@example.test"
	const displayName = "Anchor in test"
	anchor, err := api.platform.Accounts.SignUp(ctx, email, "fixture-password", displayName)
	check(err)
	check(api.platform.Accounts.SetLanguage(ctx, anchor.User.ID, "en"))
	room, err := api.platform.Rooms.Start(ctx, anchor.User.ID, "Moderated stream", "")
	check(err)
	operate := func(action string, seconds int64, reasons ...string) {
		t.Helper()
		api.call(t, "/api/operation/room/streamer_op", map[string]any{
			"ty": action, "streamerId": strconv.FormatInt(anchor.User.ID, 10),
			"roomId": strconv.FormatInt(room.ID, 10), "limitTime": seconds, "reason": reasons,
		}, 200, nil)
	}
	assertDisposition := func(wantUntil, wantViolations int64) {
		t.Helper()
		var until, violations int64
		check(db.QueryRow(ctx, `SELECT moderation_until,(SELECT count(*) FROM user_violation_records WHERE user_id=$2 AND ty=6)
			FROM rooms WHERE id=$1`, room.ID, anchor.User.ID).Scan(&until, &violations))
		if until != wantUntil || violations != wantViolations {
			t.Fatalf("room disposition changed unexpectedly: %d/%d, want %d/%d", until, violations, wantUntil, wantViolations)
		}
	}
	nextMessage := func() message {
		t.Helper()
		select {
		case got := <-sent:
			if got.Key == "" || got.From != from || len(got.To) != 1 || got.To[0].Address != (notifyemail.Address{Address: email, Name: displayName}) ||
				got.Fields["anchor_name"] != displayName || got.Fields["anchor_address"] != email {
				t.Fatalf("incorrect notification fields: %+v", got)
			}
			return got
		case <-time.After(4 * time.Second):
			t.Fatal("timed out waiting for moderation email")
			return message{}
		}
	}
	operate("Disable", -1, "reason_repeated_abuse", "reason_missing_in_history")
	check(admin.DeliverRoomModerationMail(ctx, store, sender))
	if calls.Load() != 0 {
		t.Fatal("sent email without trusted source language templates")
	}
	assertDisposition(-1, 1)
	template := func(key, content string) postgres.HistoryRow {
		return postgres.HistoryRow{Source: "language_template", ID: key, Record: json.RawMessage(fmt.Sprintf(
			`{"key":%q,"en_us":%q,"ja_jp":"Japanese template","zh_cn":"Chinese template","zh_tw":"Traditional Chinese template"}`, key, content))}
	}
	rows := legacyRows(template("email_room_banned_template", "ban-template-en"), template("email_room_banned_time_template", "Permanent account ban"),
		template("email_room_banned_renew_template", "renew-template-en"), template("reason_repeated_abuse", "Repeated abuse"))
	counts, err := store.ImportLegacyHistory(ctx, rows)
	check(err)
	if counts.Changed != 4 {
		t.Fatalf("unexpected import: %+v", counts)
	}
	counts, err = store.ImportLegacyHistory(ctx, rows)
	check(err)
	if counts.Changed != 0 {
		t.Fatalf("reimport changed language templates: %+v", counts)
	}
	if err := admin.DeliverRoomModerationMail(ctx, store, sender); err == nil {
		t.Fatal("HTTP 503 was accepted as sent")
	}
	first := nextMessage()
	if first.Template != "ban-template-en" || first.Fields["ban_time"] != "Permanent account ban" || first.Fields["reason_1"] != "Repeated abuse" ||
		first.Fields["reason_2"] != "" || len(first.Fields) != 5 {
		t.Fatalf("original ban merge parameters were not used: %+v", first)
	}
	assertDisposition(-1, 1)
	operate("Enable", 0)
	assertDisposition(0, 1)
	check(admin.DeliverRoomModerationMail(ctx, replica, sender))
	if calls.Load() != 1 {
		t.Fatal("another replica bypassed backoff or sent recovery before the ban")
	}
	_, err = db.Exec(ctx, `UPDATE room_moderation_mail SET next_attempt=clock_timestamp() WHERE room_id=$1 AND action='streamer_disable'`, room.ID)
	check(err)
	_, err = store.ImportLegacyHistory(ctx, legacyRows(template("reason_repeated_abuse", "Changed after first attempt")))
	check(err)
	check(admin.DeliverRoomModerationMail(ctx, replica, sender))
	if retry := nextMessage(); !reflect.DeepEqual(retry, first) {
		t.Fatalf("retry changed delivery identity or body: %+v vs %+v", retry, first)
	}
	check(admin.DeliverRoomModerationMail(ctx, store, sender))
	recovery := nextMessage()
	if recovery.Template != "renew-template-en" || recovery.Fields["live_id"] != strconv.FormatInt(room.ID, 10) ||
		recovery.Key == first.Key || len(recovery.Fields) != 3 {
		t.Fatalf("original recovery merge parameters were not used: %+v", recovery)
	}
	check(admin.DeliverRoomModerationMail(ctx, replica, sender))
	if calls.Load() != 3 {
		t.Fatal("a completed job was sent again")
	}
	assertDisposition(0, 1)
	_, err = api.platform.Rooms.Start(ctx, anchor.User.ID, "Second stream", "")
	check(err)
	operate("Disable", 600)
	var expires int64
	check(db.QueryRow(ctx, `SELECT moderation_until FROM rooms WHERE id=$1`, room.ID).Scan(&expires))
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- api.platform.Run(runCtx) }()
	t.Cleanup(func() {
		stop()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	timed := nextMessage()
	wantEnd := time.Unix(expires, 0).In(time.FixedZone("Japan", 9*3600)).Format("01/02/2006 15:04")
	if timed.Template != "ban-template-en" || timed.Fields["ban_time"] != wantEnd {
		t.Fatalf("worker did not deliver the persisted JST deadline: %+v", timed)
	}
	for deadline := time.Now().Add(3 * time.Second); ; {
		var completed int
		check(db.QueryRow(ctx, `SELECT count(*) FROM room_moderation_mail WHERE room_id=$1 AND sent_at IS NOT NULL`, room.ID).Scan(&completed))
		if completed == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker left accepted email pending, completed %d of 3", completed)
		}
		time.Sleep(15 * time.Millisecond)
	}
	assertDisposition(expires, 2)
}
