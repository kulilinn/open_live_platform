package app_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"open_live_platform/internal/app"
	"open_live_platform/internal/stt"
)

func TestPostgresAgoraSTTRoomLifecycle(t *testing.T) {
	config, db := postgresTestDatabase(t)
	ctx := t.Context()
	config.AgoraAppID = strings.Repeat("a", 32)
	config.AgoraCertificate = strings.Repeat("b", 32)
	const notificationSecret = "test-notifications"
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var provider sync.Mutex
	var started int
	var stopped []string
	var updates []string
	base := "/api/speech-to-text/v1/projects/" + config.AgoraAppID
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, secret, ok := r.BasicAuth()
		if !ok || id != "test-api" || secret != "test-credential" {
			t.Error("Agora REST credentials missing")
		}
		provider.Lock()
		defer provider.Unlock()
		path := strings.TrimPrefix(r.URL.Path, base)
		switch {
		case path == "/join" && r.Method == http.MethodPost:
			var input struct {
				Name      string
				Languages []string
				RTCConfig struct{ ChannelName, PubBotUID, PubBotToken string }
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || !strings.HasPrefix(input.Name, "live-") || len(input.Languages) == 0 ||
				input.RTCConfig.ChannelName == "" || input.RTCConfig.PubBotUID != "101" || !strings.HasPrefix(input.RTCConfig.PubBotToken, "007") {
				t.Errorf("Agora agent malformed join payload, err = %v", err)
			}
			started++
			json.NewEncoder(w).Encode(map[string]string{"agent_id": "agent-" + strconv.Itoa(started), "status": "RUNNING"})
		case strings.HasPrefix(path, "/agents/") && r.Method == http.MethodGet:
			w.Write([]byte(`{"status":"RUNNING"}`))
		case strings.HasSuffix(path, "/update") && r.Method == http.MethodPost:
			var input struct {
				Languages       []string
				TranslateConfig struct{ Enable bool }
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || len(input.Languages) != 1 || input.Languages[0] != "ja-JP" || input.TranslateConfig.Enable {
				t.Errorf("Agora update body = %+v, %v", input, err)
			}
			updates = append(updates, r.URL.Query().Get("sequenceId"))
			w.Write([]byte(`{"status":"RUNNING"}`))
		case strings.HasSuffix(path, "/leave") && r.Method == http.MethodPost:
			stopped = append(stopped, strings.TrimSuffix(strings.TrimPrefix(path, "/agents/"), "/leave"))
			w.Write([]byte(`{"status":"STOPPED"}`))
		default:
			t.Errorf("unexpected Agora request: %s %s", r.Method, path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(mock.Close)
	config.Subtitles = stt.Config{BaseURL: mock.URL, APIID: "test-api", APISecret: "test-credential", CallbackSecret: notificationSecret}
	start := func() *app.App {
		t.Helper()
		platform, err := app.New(ctx, config)
		check(err)
		t.Cleanup(platform.Close)
		return platform
	}
	first, peer := start(), start()
	owner, err := first.Accounts.SignUp(ctx, "stt-owner@example.org", "password", "Owner")
	check(err)
	viewer, err := first.Accounts.SignUp(ctx, "stt-viewer@example.org", "password", "Viewer")
	check(err)
	room, err := first.Rooms.Start(ctx, owner.User.ID, "Live", "")
	check(err)
	roomID := strconv.FormatInt(room.ID, 10)
	request := func(platform *app.App, method, action, token, body string, httpCode int, status string) map[string]any {
		t.Helper()
		path := "/api/live/room/stt-" + action + "/" + roomID
		incoming := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
		incoming.Header.Set("Authorization", "Bearer "+token)
		incoming.Header.Set("X-Lr-Stt-Lang", "ja-JP")
		r := httptest.NewRecorder()
		platform.Server.Handler.ServeHTTP(r, incoming)
		var result map[string]any
		check(json.NewDecoder(r.Body).Decode(&result))
		if r.Code != httpCode || (status != "" && result["status"] != status) {
			t.Fatalf("STT %s = %d %+v, want %d %q", action, r.Code, result, httpCode, status)
		}
		return result
	}
	request(first, http.MethodPost, "start", viewer.Token, `[]`, http.StatusForbidden, "403")
	if got := request(peer, http.MethodGet, "query", viewer.Token, "", http.StatusOK, "200"); got["data"] != "STOPPED" {
		t.Fatalf("initial query = %+v", got)
	}
	request(first, http.MethodPost, "start", owner.Token, `["ja-JP","en-US"]`, http.StatusOK, "200")
	request(peer, http.MethodPost, "start", owner.Token, `[]`, http.StatusOK, "200")
	var roomTaskLiveID int64
	check(db.QueryRow(ctx, `SELECT live_id FROM room_stt_tasks WHERE room_id=$1`, room.ID).Scan(&roomTaskLiveID))
	if roomTaskLiveID != room.Session.LiveID {
		t.Fatalf("persisted STT belongs to live %d, want %d", roomTaskLiveID, room.Session.LiveID)
	}
	first.Close()
	first = start()
	if got := request(first, http.MethodGet, "query", viewer.Token, "", http.StatusOK, "200"); got["data"] != "RUNNING" {
		t.Fatalf("restarted query = %+v", got)
	}
	request(first, http.MethodPost, "update", owner.Token, "", http.StatusOK, "200")
	request(peer, http.MethodPost, "update", owner.Token, "", http.StatusOK, "200")
	request(peer, http.MethodPost, "stop", viewer.Token, "", http.StatusForbidden, "403")

	callback := func(agentID string, valid bool, want int) {
		t.Helper()
		body, err := json.Marshal(map[string]any{"sid": agentID, "productId": 20, "eventType": 102, "payload": map[string]string{
			"agent_id": agentID, "channel": roomID, "status": "STOPPED",
		}})
		check(err)
		incoming := httptest.NewRequest(http.MethodPost, "/api/live/stt/callback", strings.NewReader(string(body))).WithContext(ctx)
		if valid {
			mac := hmac.New(sha256.New, []byte(notificationSecret))
			mac.Write(body)
			incoming.Header.Set("Agora-Signature-V2", hex.EncodeToString(mac.Sum(nil)))
		}
		r := httptest.NewRecorder()
		peer.Server.Handler.ServeHTTP(r, incoming)
		if r.Code != want {
			t.Fatalf("Agora callback returned %d, want %d", r.Code, want)
		}
	}
	callback("agent-1", false, http.StatusUnauthorized)
	callback("different-agent", true, http.StatusOK)
	if got := request(first, http.MethodGet, "query", viewer.Token, "", http.StatusOK, "200"); got["data"] != "RUNNING" {
		t.Fatalf("callback for old agent changed state: %+v", got)
	}
	callback("agent-1", true, http.StatusOK)
	callback("agent-1", true, http.StatusOK)
	if got := request(first, http.MethodGet, "query", viewer.Token, "", http.StatusOK, "200"); got["data"] != "STOPPED" {
		t.Fatalf("callback did not remove ended agent: %+v", got)
	}
	request(first, http.MethodPost, "start", owner.Token, `[]`, http.StatusOK, "200")
	request(peer, http.MethodPost, "stop", owner.Token, "", http.StatusOK, "200")
	request(peer, http.MethodPost, "stop", owner.Token, "", http.StatusOK, "200")
	request(first, http.MethodPost, "start", owner.Token, `[]`, http.StatusOK, "200")
	check(peer.Rooms.Close(ctx, room.ID, owner.User.ID))
	check(first.Subtitles.Sweep(ctx))
	check(peer.Subtitles.Sweep(ctx))
	newStart := httptest.NewRequest(http.MethodPost, "/api/live/room/start", strings.NewReader(`{"subject":"Next live","trans":true,"transLang":"ja-JP"}`)).WithContext(ctx)
	newStart.Header.Set("Authorization", "Bearer "+owner.Token)
	newRoom := httptest.NewRecorder()
	peer.Server.Handler.ServeHTTP(newRoom, newStart)
	if newRoom.Code != http.StatusOK {
		t.Fatalf("automatic subtitle room start HTTP = %d, %s", newRoom.Code, newRoom.Body.String())
	}
	check(db.QueryRow(ctx, `SELECT live_id FROM room_stt_tasks WHERE room_id=$1`, room.ID).Scan(&roomTaskLiveID))
	if roomTaskLiveID == room.Session.LiveID {
		t.Fatal("automatic subtitles reused the prior live session")
	}
	request(peer, http.MethodPost, "stop", owner.Token, "", http.StatusOK, "200")
	provider.Lock()
	defer provider.Unlock()
	if started != 4 || strings.Join(stopped, ",") != "agent-2,agent-3,agent-4" || strings.Join(updates, ",") != "1,2" {
		t.Fatalf("Agora lifecycle = starts %d, stops %v, updates %v", started, stopped, updates)
	}
}
