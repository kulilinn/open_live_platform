package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"open_live_platform/internal/app"
	"open_live_platform/internal/live"
	"open_live_platform/internal/liveaudit"
)

func TestPostgresLiveMediaAuditLifecycleAndHistory(t *testing.T) {
	cfg, db := postgresTestDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int64
	created, stopped := make(chan string, 8), make(chan string, 8)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Key, RequestID string
			AccessKey      string `json:"accessKey"`
			App            string `json:"appId"`
			Event          string `json:"eventId"`
			ImageCallback  string `json:"imgCallback"`
			AudioCallback  string `json:"audioCallback"`
			Data           struct {
				Stream    string `json:"streamType"`
				Room      string `json:"room"`
				Frequency int    `json:"detectFrequency"`
				Agora     struct {
					Channel, AppID, Token string
					UID                   uint32
				} `json:"agoraParam"`
				Extra struct {
					Identity liveaudit.Identity `json:"passThrough"`
				} `json:"extra"`
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.AccessKey != "local-key" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/start":
			if input.App != "local-app" || input.Event != "groupchat" || input.Data.Stream != "AGORA" || input.Data.Frequency != 15 ||
				input.Data.Agora.UID != live.DefaultAnchorNID || input.Data.Agora.AppID != cfg.AgoraAppID || !strings.HasPrefix(input.Data.Agora.Token, "007") ||
				input.Data.Room != input.Data.Agora.Channel || input.Data.Room != input.Data.Extra.Identity.Channel || input.Data.Extra.Identity.AnchorID == "" ||
				input.ImageCallback != "http://callback.local/sm/video_stream/callback" || input.AudioCallback != input.ImageCallback {
				t.Error("incorrect Shumei start request")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			attempt := calls.Add(1)
			id := fmt.Sprintf("provider-session-%d", attempt)
			created <- id
			if attempt == 1 {
				fmt.Fprintf(w, `{"code":1000,"detail":{"errorCode":1001,"dupRequestId":%q}}`, id)
				return
			}
			fmt.Fprintf(w, `{"code":1100,"requestId":%q}`, id)
		case "/stop":
			stopped <- input.RequestID
			fmt.Fprint(w, `{"code":1100}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(provider.Close)
	cfg.AgoraAppID = "0123456789abcdef0123456789abcdef"
	cfg.AgoraCertificate = "abcdef0123456789abcdef0123456789"
	cfg.LiveAudit = liveaudit.Config{Enabled: true, StartURL: provider.URL + "/start", StopURL: provider.URL + "/stop", CallbackBase: "http://callback.local",
		CallbackToken: "local-audit-review", AccessKey: "local-key", AppID: "local-app", EventID: "groupchat",
		Rules: map[string]liveaudit.Rule{"groupchat:porn:adult:explicit": {Status: true, Intercept: true, Subject: "REJECT"}}}
	run := func() *app.App {
		t.Helper()
		local := cfg
		addr := func() string {
			socket, err := net.Listen("tcp", "127.0.0.1:0")
			check(err)
			defer socket.Close()
			return socket.Addr().String()
		}
		local.Addr, local.GRPCAddr = addr(), addr()
		platform, err := app.New(ctx, local)
		check(err)
		t.Cleanup(platform.Close)
		done := make(chan error, 1)
		runCtx, stop := context.WithCancel(ctx)
		go func() { done <- platform.Run(runCtx) }()
		t.Cleanup(func() {
			stop()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(4 * time.Second):
				t.Error("media audit worker failed to stop")
			}
		})
		return platform
	}
	first, second := run(), run()
	user, err := first.Accounts.SignUp(ctx, "streamer@audit.test", "password", "Streamer")
	check(err)
	room, err := first.Rooms.Start(ctx, user.User.ID, "Moderated", "")
	check(err)
	receive := func(events <-chan string) string {
		t.Helper()
		select {
		case value := <-events:
			return value
		case <-ctx.Done():
			t.Fatal("timed out waiting for upstream audit request")
			return ""
		}
	}
	awaitPersistence := func(id string) {
		t.Helper()
		for {
			var matched bool
			check(db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM live_media_audit_requests WHERE request_id=$1)`, id).Scan(&matched))
			if matched {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal("audit provider ID did not persist")
			case <-time.After(15 * time.Millisecond):
			}
		}
	}
	requestID := receive(created)
	awaitPersistence(requestID)
	callback := func(target *app.App, path, token, body string, want int) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)).WithContext(ctx)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		out := httptest.NewRecorder()
		target.Server.Handler.ServeHTTP(out, req)
		if out.Code != want || want == http.StatusOK && !strings.Contains(out.Body.String(), `"code":1100`) {
			t.Fatalf("audit callback %s got %d %s, want %d", path, out.Code, out.Body.String(), want)
		}
	}
	identity := fmt.Sprintf(`{"anchorId":"%d","channel":"%d","anchorName":"Streamer"}`, user.User.ID, room.ID)
	manual := fmt.Sprintf(`{"requestId":"review-1","result":{"operation":1,"tips":"warning"},"data":{"streamId":%q,"passThrough":%s}}`, requestID, identity)
	callback(second, liveaudit.ManualPath, cfg.LiveAudit.CallbackToken, manual, 200)
	callback(first, liveaudit.ManualPath, cfg.LiveAudit.CallbackToken, manual, 200)
	machine := func(id, label string) string {
		return fmt.Sprintf(`{"requestId":%q,"code":1100,"statCode":0,"contentType":1,"auxInfo":{"passThrough":%s},
			"frameDetail":{"riskLevel":"REVIEW","riskLabel1":"porn","riskLabel2":"adult","riskLabel3":%q}}`, id, identity, label)
	}
	callback(first, liveaudit.StreamPath, "", machine(requestID, "explicit"), 401)
	callback(first, liveaudit.StreamPath, cfg.LiveAudit.CallbackToken, machine("unrelated", "explicit"), 404)
	callback(second, liveaudit.StreamPath, cfg.LiveAudit.CallbackToken, machine(requestID, "no-rule"), 200)
	info, err := first.Rooms.Info(ctx, room.ID)
	check(err)
	if !info.Online {
		t.Fatal("an unconfigured label blocked the stream")
	}
	callback(second, liveaudit.StreamPath, cfg.LiveAudit.CallbackToken, machine(requestID, "explicit"), 200)
	info, err = first.Rooms.Info(ctx, room.ID)
	check(err)
	if info.Online || receive(stopped) != requestID {
		t.Fatal("verified moderation did not close the protected broadcast and stop the upstream task")
	}
	_, err = db.Exec(ctx, `UPDATE rooms SET moderation_until=0,moderation_total=0,moderation_reason='' WHERE id=$1`, room.ID)
	check(err)
	restarted, err := second.Rooms.Start(ctx, user.User.ID, "New broadcast", "")
	check(err)
	newID := receive(created)
	awaitPersistence(newID)
	callback(first, liveaudit.StreamPath, cfg.LiveAudit.CallbackToken, machine(requestID, "explicit")+" ", 200)
	info, err = first.Rooms.Info(ctx, room.ID)
	check(err)
	if !info.Online || info.Session.LiveID != restarted.Session.LiveID || calls.Load() != 2 {
		t.Fatalf("stale moderation affected the new broadcast: %+v, provider starts=%d", info, calls.Load())
	}
	check(first.Rooms.Close(ctx, room.ID, user.User.ID))
	if id := receive(stopped); id != newID {
		t.Fatalf("stopped wrong provider task: %s", id)
	}
	var history, applied, violations, notices int
	check(db.QueryRow(ctx, `SELECT (SELECT count(*) FROM live_media_audit_history WHERE request_id=$1),
		(SELECT count(*) FROM live_media_audit_history WHERE request_id=$1 AND applied),
		(SELECT count(*) FROM user_violation_records WHERE user_id=$2),
		(SELECT count(*) FROM notices WHERE receive_id=$2 AND notice_type=12)`, requestID, user.User.ID).Scan(&history, &applied, &violations, &notices))
	if history != 4 || applied != 2 || violations != 2 || notices != 1 {
		t.Fatalf("audit history=%d applied=%d violations=%d notices=%d", history, applied, violations, notices)
	}
}
