package app_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/relay"
	"open_live_platform/internal/stt"
)

func TestPostgresDateSessionLargeIDsPermissionsAndSubtitles(t *testing.T) {
	config, db := postgresTestDatabase(t)
	ctx := t.Context()
	const anchor, fan, stranger = int64(9_007_199_254_740_995), int64(9_007_199_254_740_997), int64(9_007_199_254_740_999)
	const anchorToken, fanToken, strangerToken = "test-date-anchor", "test-date-fan", "test-date-stranger"
	anchorID, fanID := strconv.FormatInt(anchor, 10), strconv.FormatInt(fan, 10)
	config.AgoraAppID, config.AgoraCertificate = strings.Repeat("a", 32), strings.Repeat("b", 32)
	config.RelayRust = relay.RustOptions{AppKey: "app", AppSecret: "test-relay-secret"}
	var balance atomic.Int64
	var subtitlesStarted, subtitlesStopped atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/payment/user/point" {
			if r.Header.Get("USER_ID") != fanID || r.Header.Get("APPID") != "payments" {
				t.Error("date balance request used the wrong user or project")
			}
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": map[string]int64{"point": balance.Load()}})
			return
		}
		base := "/api/speech-to-text/v1/projects/" + config.AgoraAppID
		switch strings.TrimPrefix(r.URL.Path, base) {
		case "/join":
			var body struct {
				Languages []string
				RTCConfig struct{ ChannelName, PubBotToken string }
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Languages) != 2 ||
				!strings.HasPrefix(body.RTCConfig.PubBotToken, "007") || len(body.RTCConfig.ChannelName) < 18 {
				t.Errorf("invalid 1v1 STT start, error = %v", err)
			}
			subtitlesStarted.Add(1)
			w.Write([]byte(`{"agent_id":"date-test-agent","status":"RUNNING"}`))
		case "/agents/date-test-agent":
			w.Write([]byte(`{"status":"RUNNING"}`))
		case "/agents/date-test-agent/update":
			w.Write([]byte(`{"status":"RUNNING"}`))
		case "/agents/date-test-agent/leave":
			subtitlesStopped.Add(1)
			w.Write([]byte(`{"status":"STOPPED"}`))
		default:
			t.Errorf("unexpected provider request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(provider.Close)
	config.PaymentHost, config.PaymentAppID = provider.URL, "payments"
	config.Subtitles = stt.Config{BaseURL: provider.URL, APIID: "agora-test-key", APISecret: "agora-test-secret"}
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	open := func() (*app.App, *httptest.Server) {
		t.Helper()
		platform, err := app.New(ctx, config)
		check(err)
		t.Cleanup(platform.Close)
		server := httptest.NewServer(platform.Server.Handler)
		t.Cleanup(server.Close)
		return platform, server
	}
	first, firstServer := open()
	_, secondServer := open()
	_, err := db.Exec(ctx, `INSERT INTO users(id,email,account,password_salt,password_hash,profile) VALUES
		($1,'anchor@example.org','Anchor','','',$4),($2,'fan@example.org','Fan','','',NULL),($3,'other@example.org','Other','','',NULL)`,
		anchor, fan, stranger, auth.Profile{IsAnchor: true, Name: "Host", Photo: "photo", ToyInfo: `{}`})
	check(err)
	_, err = db.Exec(ctx, `INSERT INTO sessions(token,user_id) VALUES ($4,$1),($5,$2),($6,$3)`,
		anchor, fan, stranger, anchorToken, fanToken, strangerToken)
	check(err)
	request := func(server *httptest.Server, method, path, token, lang, body string, code int, status string) map[string]any {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, server.URL+path, strings.NewReader(body))
		check(err)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Lr-Stt-Lang", lang)
		res, err := server.Client().Do(req)
		check(err)
		defer res.Body.Close()
		var data map[string]any
		check(json.NewDecoder(res.Body).Decode(&data))
		if res.StatusCode != code || (status != "" && data["status"] != status) {
			t.Fatalf("%s %s = %d %+v, want %d %s", method, path, res.StatusCode, data, code, status)
		}
		return data
	}
	connect := func(server *httptest.Server, token string) *websocket.Conn {
		t.Helper()
		ticket := request(server, http.MethodGet, "/api/device/qrcode", token, "", "", 200, "200")
		parsed, err := url.Parse(ticket["url"].(string))
		check(err)
		dialer := websocket.Dialer{Subprotocols: []string{"h5relayer"}}
		conn, res, err := dialer.DialContext(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+parsed.Path, nil)
		if err != nil && res != nil {
			res.Body.Close()
		}
		check(err)
		t.Cleanup(func() { conn.Close() })
		check(conn.SetReadDeadline(time.Now().Add(6 * time.Second)))
		return conn
	}
	read := func(conn *websocket.Conn, name, queue string) map[string]any {
		t.Helper()
		for {
			var event struct {
				Content string
				Queue   string
				Payload map[string]any
			}
			check(conn.ReadJSON(&event))
			if event.Content == name {
				if event.Queue != queue {
					t.Fatalf("private %s queue = %s, want %s", name, event.Queue, queue)
				}
				return event.Payload
			}
		}
	}
	request(firstServer, http.MethodPost, "/api/live/start", strangerToken, "", "", 403, "403")
	request(firstServer, http.MethodPost, "/api/live/base-setting", anchorToken, "", `{"datingCosts":{"time":"3","cost":2}}`, 200, "200")
	request(firstServer, http.MethodPost, "/api/live/start", anchorToken, "", "", 200, "200")
	request(secondServer, http.MethodPost, "/api/live/callin/"+anchorID, fanToken, "en-US", "", 200, "4023")
	_, err = db.Exec(ctx, `WITH plan AS (INSERT INTO subscription_plans(anchor_id,plan_type,price) VALUES($1,4,10) RETURNING id)
		INSERT INTO subscription_members(fan_id,anchor_id,plan_id,expires_at) SELECT $2,$1,id,statement_timestamp()+interval '1 day' FROM plan`, anchor, fan)
	check(err)
	request(secondServer, http.MethodPost, "/api/live/callin/"+anchorID, fanToken, "en-US", "", 200, "4002")
	balance.Store(100)
	anchorSocket, fanSocket := connect(firstServer, anchorToken), connect(secondServer, fanToken)
	request(secondServer, http.MethodPost, "/api/live/callin/"+anchorID, fanToken, "en-US", "", 200, "200")
	if got := read(anchorSocket, "callin", anchorID); got["uid"] != fanID {
		t.Fatalf("invitation lost high ID: %+v", got)
	}
	request(secondServer, http.MethodGet, "/api/live/callins/"+anchorID, strangerToken, "", "", 403, "403")
	request(firstServer, http.MethodPost, "/api/live/callout/"+fanID, anchorToken, "ja-JP", "", 200, "200")
	hostRTC, fanRTC := read(anchorSocket, "callout", anchorID), read(fanSocket, "callout", fanID)
	channel, _ := hostRTC["channel"].(string)
	if len(channel) < 18 || fanRTC["channel"] != channel || hostRTC["uid"] != anchorID || fanRTC["uid"] != fanID ||
		hostRTC["nid"] != "100001" || fanRTC["nid"] != "100002" || hostRTC["remoteUid"] != fanID || fanRTC["remoteUid"] != anchorID {
		t.Fatalf("incorrect date RTC identities: room %q and NIDs %v/%v", channel, hostRTC["nid"], fanRTC["nid"])
	}
	request(secondServer, http.MethodGet, "/api/live/token/"+channel+"/1", fanToken, "", "", 403, "403")
	request(secondServer, http.MethodGet, "/api/live/token/"+channel+"/2", strangerToken, "", "", 403, "403")
	if got := request(secondServer, http.MethodGet, "/api/live/token/"+channel+"/2", fanToken, "", "", 200, "200"); got["data"].(map[string]any)["nid"] != "100002" {
		t.Fatal("renewal changed audience RTC identity")
	}
	request(firstServer, http.MethodGet, "/api/live/duration/"+channel, anchorToken, "", "", 200, "200")
	request(secondServer, http.MethodGet, "/api/live/duration/"+channel, fanToken, "", "", 200, "200")
	request(secondServer, http.MethodPost, "/api/live/stt-start/"+channel, strangerToken, "fr-FR", `[]`, 403, "403")
	request(secondServer, http.MethodPost, "/api/live/stt-start/"+channel, fanToken, "en-US", `[]`, 200, "200")
	request(firstServer, http.MethodPost, "/api/live/stt-update/"+channel, anchorToken, "ko-KR", "", 200, "200")
	request(firstServer, http.MethodGet, "/api/live/stt-query/"+channel, anchorToken, "", "", 200, "200")
	request(secondServer, http.MethodPost, "/api/live/leave/"+channel, strangerToken, "", "", 403, "403")
	request(secondServer, http.MethodPost, "/api/live/leave/"+channel, fanToken, "", "", 200, "200")
	if subtitlesStarted.Load() != 1 || subtitlesStopped.Load() != 1 {
		t.Fatal("ending 1v1 did not stop the corresponding subtitle task")
	}
	request(firstServer, http.MethodGet, "/api/live/token/"+channel+"/1", anchorToken, "", "", 200, "4022")
	checkout := request(firstServer, http.MethodGet, "/api/live/checkout/"+channel, anchorToken, "", "", 200, "200")
	if checkout["data"].(map[string]any)["points"] != float64(6) {
		t.Fatalf("checkout was not based on agreed rate: %+v", checkout)
	}
	request(firstServer, http.MethodGet, "/api/live/checkout/"+channel, strangerToken, "", "", 403, "403")
	first.Close()
	_, firstServer = open()
	request(firstServer, http.MethodGet, "/api/live/checkout/"+channel, anchorToken, "", "", 200, "200")
	request(secondServer, http.MethodPost, "/api/live/callin/"+anchorID, fanToken, "en-US", "", 200, "200")
	request(firstServer, http.MethodPost, "/api/live/callout/"+fanID, anchorToken, "ja-JP", "", 200, "200")
	var newChannel int64
	check(db.QueryRow(ctx, `SELECT channel FROM date_sessions WHERE ended_at IS NULL`).Scan(&newChannel))
	if strconv.FormatInt(newChannel, 10) == channel {
		t.Fatal("new date reused its previous RTC channel")
	}
	request(firstServer, http.MethodPost, "/api/live/stop", anchorToken, "", "", 200, "200")
}
