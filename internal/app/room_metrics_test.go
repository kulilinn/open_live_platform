package app_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/live"
)

func TestRoomMetricsAndRelayRoutes(t *testing.T) {
	t.Run("memory", func(t *testing.T) { testRoomMetricsAndRelayRoutes(t, app.Config{RelaySecret: "room-metrics"}, nil) })
	t.Run("postgres", func(t *testing.T) {
		cfg, db := postgresTestDatabase(t)
		testRoomMetricsAndRelayRoutes(t, cfg, db)
	})
}

func testRoomMetricsAndRelayRoutes(t *testing.T, cfg app.Config, db *pgx.Conn) {
	t.Helper()
	ctx := t.Context()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	open := func() *app.App {
		platform, err := app.New(ctx, cfg)
		check(err)
		t.Cleanup(platform.Close)
		return platform
	}
	first := open()
	peer := first
	if cfg.DatabaseURL != "" {
		peer = open()
	}
	signUp := func(name string) auth.Session {
		session, err := first.Accounts.SignUp(ctx, name+"@metrics.test", "password", name)
		check(err)
		return session
	}
	host, viewer, fan := signUp("anchor"), signUp("viewer"), signUp("fan")
	banner := "banner.png"
	room, err := first.Rooms.StartWithSettings(ctx, host.User.ID, live.RoomSettings{Banner: &banner}, false)
	check(err)
	roomID := strconv.FormatInt(room.ID, 10)
	liveID := strconv.FormatInt(room.Session.LiveID, 10)
	check(first.Rooms.Join(ctx, room.ID, viewer.User.ID, ""))
	check(peer.Rooms.Join(ctx, room.ID, fan.User.ID, ""))
	check(peer.Rooms.Leave(ctx, room.ID, fan.User.ID))
	request := func(a *app.App, method, path, token, body string, wantHTTP int, wantStatus string) json.RawMessage {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		a.Server.Handler.ServeHTTP(w, r)
		var response struct {
			Status string
			Data   json.RawMessage
		}
		if w.Code != wantHTTP {
			t.Fatalf("%s %s returned %d (%s), want %d", method, path, w.Code, w.Body.String(), wantHTTP)
		}
		if wantStatus != "" {
			check(json.NewDecoder(w.Body).Decode(&response))
			if response.Status != wantStatus {
				t.Fatalf("%s %s status=%s, want %s", method, path, response.Status, wantStatus)
			}
		}
		return response.Data
	}
	request(first, "GET", "/api/device/toyinfo", "", "", 401, "")
	request(first, "GET", "/api/device/toyinfo", viewer.Token, "", 200, "200")
	request(peer, "GET", "/relay/verify/unknown/invalid", "", "", 403, "")
	request(first, "POST", "/api/live/room/sync_watch_time", "", "", 401, "")
	request(first, "POST", "/api/live/room/sync_watch_time", viewer.Token, "", 200, "200")
	request(peer, "DELETE", "/api/live/room/report/"+roomID, viewer.Token,
		`{"reporterID":8,"room":"999","subject":"display","description":"report text"}`, 200, "200")
	if db != nil {
		var id, author, subject, description string
		check(db.QueryRow(ctx, `SELECT room,reporter_id::text,subject,description FROM room_reports`).Scan(&id, &author, &subject, &description))
		if id != roomID || author != strconv.FormatInt(viewer.User.ID, 10) || subject != "display" || description != "report text" {
			t.Fatalf("saved room report = %q %q %q %q", id, author, subject, description)
		}
		_, err = db.Exec(ctx, `WITH p AS (INSERT INTO subscription_plans(anchor_id,plan_type,price) VALUES($1,2,0) RETURNING id)
			INSERT INTO subscription_members(anchor_id,fan_id,plan_id,expires_at)
			SELECT $1,$2,id,statement_timestamp()+interval '1 day' FROM p`, host.User.ID, fan.User.ID)
		check(err)
	}
	anchor := request(first, "GET", "/api/live/room/anchor/"+roomID, viewer.Token, "", 200, "200")
	var identity struct{ ID, NID, DisplayName string }
	check(json.Unmarshal(anchor, &identity))
	if identity.ID != roomID || identity.NID != strconv.FormatUint(uint64(live.DefaultAnchorNID), 10) || identity.DisplayName != "anchor" {
		t.Fatalf("anchor = %+v", identity)
	}

	server := httptest.NewServer(peer.Server.Handler)
	t.Cleanup(server.Close)
	lookup, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/device/qrcode", nil)
	check(err)
	lookup.Header.Set("Authorization", "Bearer "+host.Token)
	response, err := server.Client().Do(lookup)
	check(err)
	var ticket struct{ URL string }
	check(json.NewDecoder(response.Body).Decode(&ticket))
	response.Body.Close()
	dialer := websocket.Dialer{Subprotocols: []string{"h5relayer"}, HandshakeTimeout: 5 * time.Second}
	socket, _, err := dialer.DialContext(ctx, ticket.URL+"?room_id="+roomID, nil)
	check(err)
	t.Cleanup(func() { socket.Close() })
	check(socket.SetReadDeadline(time.Now().Add(5 * time.Second)))
	type roomFrame struct {
		Content string
		Payload []struct {
			Channel string
			Message struct {
				Kind string `json:"msgType"`
				Data struct {
					Room      string `json:"roomId"`
					Live      string `json:"liveId"`
					Sender    string `json:"thumbPeople"`
					LikeCount int64  `json:"curentLikeCount"`
				} `json:"message"`
			}
		}
	}
	var greeting roomFrame
	check(socket.ReadJSON(&greeting))
	like := func(platform *app.App, session auth.Session, id string, count int64, httpStatus int, status string) string {
		return string(request(platform, "POST", "/api/live/room/live/like", session.Token,
			fmt.Sprintf(`{"roomId":%q,"liveId":%q,"likeCount":%d}`, roomID, id, count), httpStatus, status))
	}
	if got := like(first, viewer, liveID, 2, 200, "200"); got != "2" {
		t.Fatalf("first like total = %s", got)
	}
	var got roomFrame
	check(socket.ReadJSON(&got))
	if got.Content != "live_chat" || len(got.Payload) != 1 || got.Payload[0].Message.Kind != "Z" ||
		got.Payload[0].Message.Data.LikeCount != 2 || got.Payload[0].Channel != "room_event" ||
		got.Payload[0].Message.Data.Room != roomID || got.Payload[0].Message.Data.Live != liveID ||
		got.Payload[0].Message.Data.Sender != strconv.FormatInt(viewer.User.ID, 10) {
		t.Fatalf("H5 like frame = %+v", got)
	}
	if got := like(peer, fan, liveID, 3, 200, "200"); got != "5" {
		t.Fatalf("second like total = %s", got)
	}
	like(peer, fan, liveID, -1, 400, "")
	like(peer, fan, "9223372036854775807", 1, 409, "")
	settle := func(who auth.Session, expects int, status string) live.RoomSettlement {
		t.Helper()
		raw := request(peer, "GET", "/api/live/room/settle/"+liveID, who.Token, "", expects, status)
		var data live.RoomSettlement
		if status == "200" {
			check(json.Unmarshal(raw, &data))
		}
		return data
	}
	settle(viewer, 403, "")
	var newFans int64
	if db != nil {
		newFans = 1
	}
	if data := settle(host, 200, "200"); data.FanCount != 2 || data.Banner != banner || data.NewFanCount != newFans {
		t.Fatalf("active room settlements = %+v", data)
	}
	check(peer.Rooms.Close(ctx, room.ID, host.User.ID))
	like(peer, viewer, liveID, 1, 409, "")
	second, err := first.Rooms.Start(ctx, host.User.ID, "Restarted", "")
	check(err)
	if data := settle(host, 200, "200"); data.FanCount != 2 || data.Banner != banner {
		t.Fatalf("old room settlements = %+v", data)
	}
	if got := like(first, viewer, strconv.FormatInt(second.Session.LiveID, 10), 1, 200, "200"); got != "1" {
		t.Fatalf("new broadcast like total = %s", got)
	}
}

func TestPostgresDateRecommendationsByRecentAccepting(t *testing.T) {
	cfg, db := postgresTestDatabase(t)
	ctx := t.Context()
	first, err := app.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(first.Close)
	second, err := app.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Close)
	signUp := func(name string) auth.Session {
		session, err := first.Accounts.SignUp(ctx, name+"@recommend.test", "password", name)
		if err != nil {
			t.Fatal(err)
		}
		return session
	}
	firstHost, secondHost, viewer := signUp("first"), signUp("second"), signUp("viewer")
	for _, host := range []auth.Session{firstHost, secondHost} {
		if _, err := db.Exec(ctx, "UPDATE users SET profile=$2 WHERE id=$1", host.User.ID, auth.Profile{IsAnchor: true, Name: host.User.Account}); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/api/live/start", nil).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+host.Token)
		w := httptest.NewRecorder()
		first.Server.Handler.ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"200"`) {
			t.Fatalf("enable 1v1 = %d: %s", w.Code, w.Body.String())
		}
	}
	if _, err := db.Exec(ctx, `UPDATE date_hosts SET state=jsonb_set(state,'{AcceptUntil}','"2001-01-01T00:00:00Z"') WHERE anchor_id=$1`, firstHost.User.ID); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/live/anchor/recommends", nil).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+viewer.Token)
	w := httptest.NewRecorder()
	second.Server.Handler.ServeHTTP(w, r)
	var response struct {
		Status string
		Data   []struct {
			ID                   string
			AnchorStatus, Online int
		}
	}
	err = json.NewDecoder(w.Body).Decode(&response)
	if err != nil || w.Code != 200 || response.Status != "200" || len(response.Data) != 2 {
		t.Fatalf("recommendations = %d %+v: %v", w.Code, response, err)
	}
	if response.Data[0].ID != strconv.FormatInt(secondHost.User.ID, 10) || response.Data[0].AnchorStatus != 1 ||
		response.Data[1].ID != strconv.FormatInt(firstHost.User.ID, 10) || response.Data[1].Online != 0 {
		t.Fatalf("recommendation order and states = %+v", response.Data)
	}
}
