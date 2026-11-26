package app_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
)

func TestRoomSettingsAndH5Translations(t *testing.T) {
	testRoomSettingsAndH5Translations(t, app.Config{RelaySecret: "translation-test"}, false)
}

func TestPostgresRoomSettingsAndH5Translations(t *testing.T) {
	cfg, _ := postgresTestDatabase(t)
	cfg.RelaySecret = "translation-test"
	testRoomSettingsAndH5Translations(t, cfg, true)
}

func testRoomSettingsAndH5Translations(t *testing.T, cfg app.Config, acrossInstances bool) {
	t.Helper()
	ctx := t.Context()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	open := func() *app.App {
		t.Helper()
		platform, err := app.New(ctx, cfg)
		check(err)
		t.Cleanup(platform.Close)
		return platform
	}
	writer := open()
	viewerPlatform := writer
	if acrossInstances {
		viewerPlatform = open()
	}
	newHTTP := func(platform *app.App) *httptest.Server {
		t.Helper()
		server := httptest.NewServer(platform.Server.Handler)
		t.Cleanup(server.Close)
		return server
	}
	api, relay := newHTTP(writer), newHTTP(viewerPlatform)
	signUp := func(account string) auth.Session {
		t.Helper()
		session, err := writer.Accounts.SignUp(ctx, account+"@example.test", "password", account)
		check(err)
		return session
	}
	owner, watcher, visitor := signUp("owner"), signUp("watcher"), signUp("visitor")
	roomID := strconv.FormatInt(owner.User.ID, 10)
	request := func(server *httptest.Server, method, path string, session auth.Session, body string, wantStatus int, result any) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, server.URL+path, strings.NewReader(body))
		check(err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+session.Token)
		res, err := server.Client().Do(req)
		check(err)
		defer res.Body.Close()
		if res.StatusCode != wantStatus {
			t.Fatalf("%s %s returned %d, want %d", method, path, res.StatusCode, wantStatus)
		}
		if result != nil {
			check(json.NewDecoder(res.Body).Decode(result))
		}
	}
	const base = "/api/live/room/"
	const compat = "?compat=rust"
	type roomInfo struct {
		Room, Anchor, LiveID, Subject, Banner, Tag, Description, OfflineInfo, TransLang string
		Online                                                                          int
		Trans, IsLock                                                                   bool
		Plan, Gifts                                                                     []string
	}
	type roomResult struct {
		Status, Message string
		Data            roomInfo
	}
	var configured roomResult
	request(api, http.MethodPost, base+"setting"+compat, owner,
		`{"subject":"Room","banner":"cover.png","tag":"music","description":"Inside","offlineInfo":"Later","plan":["2","10"],"gifts":["3","1"],"trans":true,"transLang":"ja","password":"secret"}`,
		http.StatusOK, &configured)
	assertRoom := func(result roomResult, online int, locked bool) {
		t.Helper()
		d := result.Data
		if result.Status != "200" || d.Room != roomID || d.Anchor != roomID || d.Subject != "Room" || d.Banner != "cover.png" ||
			d.Tag != "music" || d.Description != "Inside" || d.OfflineInfo != "Later" || d.Online != online || d.IsLock != locked ||
			!d.Trans || d.TransLang != "ja" || !slices.Equal(d.Plan, []string{"10", "2"}) || !slices.Equal(d.Gifts, []string{"3", "1"}) {
			t.Fatalf("room metadata = %+v", result)
		}
	}
	assertRoom(configured, 0, true)
	if configured.Data.LiveID != "0" {
		t.Fatalf("configuring offline created live id %q", configured.Data.LiveID)
	}
	var missing roomResult
	request(api, http.MethodGet, base+"info/999999"+compat, owner, "", http.StatusOK, &missing)
	if missing.Status != "4005" || missing.Message != "data not found " {
		t.Fatalf("missing room = %+v", missing)
	}
	var list struct {
		Status string
		Data   []roomInfo
	}
	request(api, http.MethodPost, base+"list"+compat, watcher,
		`{"page":1,"pageSize":1,"tag":"music"}`, http.StatusOK, &list)
	if list.Status != "200" || len(list.Data) != 1 || list.Data[0].Banner != "cover.png" {
		t.Fatalf("tagged room list = %+v", list)
	}
	var started roomResult
	request(api, http.MethodPost, base+"start"+compat, owner, `{"banner":""}`, http.StatusOK, &started)
	assertRoom(started, 1, false)
	if started.Data.LiveID == "0" || started.Data.LiveID == "" {
		t.Fatalf("started live id = %q", started.Data.LiveID)
	}
	var duplicate roomResult
	request(api, http.MethodPost, base+"start"+compat, owner, `{"description":"wrong"}`, http.StatusOK, &duplicate)
	if duplicate.Status != "4079" {
		t.Fatalf("duplicate start = %+v", duplicate)
	}
	if acrossInstances {
		reopened := newHTTP(open())
		var stored roomResult
		request(reopened, http.MethodGet, base+"info/"+roomID+compat, watcher, "", http.StatusOK, &stored)
		assertRoom(stored, 1, false)
		if stored.Data.LiveID != started.Data.LiveID {
			t.Fatalf("stored live id = %q, want %q", stored.Data.LiveID, started.Data.LiveID)
		}
	}
	request(api, http.MethodPost, base+"join/"+roomID, watcher, "", http.StatusNoContent, nil)
	var ticket struct{ URL string }
	request(relay, http.MethodGet, "/api/device/qrcode", watcher, "", http.StatusOK, &ticket)
	dialer := websocket.Dialer{Subprotocols: []string{"h5relayer"}, HandshakeTimeout: 5 * time.Second}
	conn, _, err := dialer.DialContext(ctx, ticket.URL+"?room_id="+roomID, nil)
	check(err)
	t.Cleanup(func() { conn.Close() })
	check(conn.SetReadDeadline(time.Now().Add(5 * time.Second)))
	var greeting struct{ Content string }
	check(conn.ReadJSON(&greeting))
	if greeting.Content != "control" {
		t.Fatalf("H5 greeting = %+v", greeting)
	}
	type h5Payload struct {
		ID, Channel, Room, Sender, Timestamp string
		Priority                             *int
		Message                              struct {
			MsgType  string          `json:"msgType"`
			MsgGroup string          `json:"msgGroup"`
			Data     json.RawMessage `json:"message"`
			UserInfo map[string]json.RawMessage
		}
	}
	nextPayload := func() h5Payload {
		t.Helper()
		check(conn.SetReadDeadline(time.Now().Add(5 * time.Second)))
		for {
			var frame struct {
				Content, Queue string
				Payload        []h5Payload
			}
			check(conn.ReadJSON(&frame))
			if frame.Content != "live_chat" {
				continue
			}
			if frame.Queue != roomID || len(frame.Payload) != 1 {
				t.Fatalf("H5 frame = %+v", frame)
			}
			return frame.Payload[0]
		}
	}
	expectLanguages := func(want ...string) {
		t.Helper()
		p := nextPayload()
		var got []string
		check(json.Unmarshal(p.Message.Data, &got))
		id, idErr := strconv.ParseInt(p.ID, 10, 64)
		timestamp, timeErr := strconv.ParseInt(p.Timestamp, 10, 64)
		if p.Channel != "translate_change" || p.Room != roomID || p.Sender != roomID || p.Priority == nil || *p.Priority != 0 ||
			p.Message.MsgType != "T" || p.Message.MsgGroup != "chat" || string(p.Message.UserInfo["uid"]) != roomID ||
			string(p.Message.UserInfo["role"]) != "5" || idErr != nil || id < 1 || timeErr != nil || timestamp < 1 ||
			got == nil || !slices.Equal(got, want) {
			t.Fatalf("H5 T payload = %+v, languages = %v, want %v", p, got, want)
		}
	}
	setUser := func(who auth.Session, data string) {
		t.Helper()
		request(api, http.MethodPost, "/api/user/set_info", who, data, http.StatusOK, nil)
	}
	setUser(visitor, `{"transLang":"fr"}`)
	var joined struct{ Data struct{ Languages []string } }
	request(api, http.MethodPost, base+"join/"+roomID+compat, visitor, "", http.StatusOK, &joined)
	if !slices.Equal(joined.Data.Languages, []string{"en", "fr"}) {
		t.Fatalf("joined languages = %v", joined.Data.Languages)
	}
	expectLanguages("en", "fr")
	setUser(visitor, `{"trans":true,"transLang":"es"}`)
	expectLanguages("en", "es")
	request(api, http.MethodPost, base+"setting"+compat, owner, `{"transLang":"ja-JP"}`, http.StatusOK, nil)
	expectLanguages("en", "es", "ja-JP")
	request(api, http.MethodPost, base+"setting"+compat, owner, `{"trans":false}`, http.StatusOK, nil)
	expectLanguages("en", "es")
	request(api, http.MethodPost, base+"leave/"+roomID+compat, visitor, "", http.StatusOK, nil)
	expectLanguages("en")
	setUser(watcher, `{"transLang":"de"}`)
	request(api, http.MethodPost, base+"chat", owner, `{"room_id":`+roomID+`,"text":"still live"}`, http.StatusNoContent, nil)
	if p := nextPayload(); p.Message.MsgType != "C" {
		t.Fatalf("changing a disabled viewer language emitted %+v before the chat", p)
	}
	setUser(watcher, `{"trans":true}`)
	expectLanguages("de")
	setUser(watcher, `{"trans":false}`)
	expectLanguages()
}
