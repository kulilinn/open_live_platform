package httpapi_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/httpapi"
	"open_live_platform/internal/live"
)

func TestCoreHTTPFlow(t *testing.T) {
	accounts := auth.NewStore()
	rooms := live.NewStore()
	mux := http.NewServeMux()
	httpapi.Register(mux, accounts, rooms)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	status := requestJSON(t, server.Client(), http.MethodGet, server.URL+"/healthchecker", "", nil)
	if status.Code != http.StatusOK || status.Body["status"] != "200" {
		t.Fatalf("health = %d %+v", status.Code, status.Body)
	}

	signup := requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/auth/signup", "", map[string]string{
		"email":    "owner@example.com",
		"password": "secret",
		"account":  "owner",
	})
	if signup.Code != http.StatusCreated {
		t.Fatalf("signup = %d %+v", signup.Code, signup.Body)
	}
	token := signup.Body["token"].(string)
	user := signup.Body["user"].(map[string]any)
	roomOwner := int64(user["id"].(float64))

	unauthorized := requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/live/room/start", "", map[string]string{"title": "Live"})
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized start = %d", unauthorized.Code)
	}

	start := requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/live/room/start", token, map[string]string{"title": "Live"})
	if start.Code != http.StatusOK || int64(start.Body["id"].(float64)) != roomOwner {
		t.Fatalf("start = %d %+v", start.Code, start.Body)
	}

	join := requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/live/room/join/"+itoa(roomOwner), token, nil)
	if join.Code != http.StatusNoContent {
		t.Fatalf("join = %d", join.Code)
	}

	chat := requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/live/room/chat", token, map[string]any{
		"room_id": roomOwner,
		"text":    "hello",
	})
	if chat.Code != http.StatusNoContent {
		t.Fatalf("chat = %d %+v", chat.Code, chat.Body)
	}

	info := requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/live/room/info/"+itoa(roomOwner), token, nil)
	if info.Code != http.StatusOK || int(info.Body["member_count"].(float64)) != 0 {
		t.Fatalf("info = %d %+v", info.Code, info.Body)
	}
}

type response struct {
	Code int
	Body map[string]any
}

func requestJSON(t *testing.T, client *http.Client, method, url, token string, value any) response {
	t.Helper()
	var body io.Reader
	if value != nil {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var decoded map[string]any
	if res.ContentLength != 0 {
		if err := json.NewDecoder(res.Body).Decode(&decoded); err != nil {
			t.Fatal(err)
		}
	}
	return response{Code: res.StatusCode, Body: decoded}
}

func itoa(value int64) string {
	return fmt.Sprintf("%d", value)
}

func TestRoomListHTTP(t *testing.T) {
	accounts, rooms := auth.NewStore(), live.NewStore()
	viewer, err := accounts.SignUp(t.Context(), "viewer@example.org", "secret", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	for id := int64(1); id <= 22; id++ {
		if _, err := rooms.Start(t.Context(), id, "Live", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := rooms.Close(t.Context(), 1, 1); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	httpapi.Register(mux, accounts, rooms)
	post := func(body, token string, want int) []byte {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/live/room/list", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("list(%q) returned %d, want %d: %s", body, rec.Code, want, rec.Body)
		}
		return rec.Body.Bytes()
	}
	post(`{"online":true}`, "", http.StatusUnauthorized)
	for _, body := range []string{"", "{}"} {
		var all []live.Room
		if err := json.Unmarshal(post(body, viewer.Token, http.StatusOK), &all); err != nil || len(all) != 22 || all[0].ID != 1 {
			t.Fatalf("legacy list(%q) = %v, %v", body, all, err)
		}
	}
	for _, tc := range []struct {
		body  string
		pages int
		count int
		first int64
		last  int64
	}{
		{`{"online":true}`, 2, 20, 2, 21},
		{`{"online":true,"page":2}`, 2, 1, 22, 22},
		{`{"online":false,"pageSize":1}`, 1, 1, 1, 1},
		{`{"page":2,"online":null}`, 2, 2, 22, 1},
		{`{"online":true,"pageSize":0}`, 21, 0, 0, 0},
		{`{"page":9223372036854775807,"pageSize":20}`, 2, 0, 0, 0},
	} {
		var result struct {
			Status string
			Data   []live.Room
			Pages  int
		}
		if err := json.Unmarshal(post(tc.body, viewer.Token, http.StatusOK), &result); err != nil {
			t.Fatal(err)
		}
		if result.Status != "200" || result.Pages != tc.pages || result.Data == nil || len(result.Data) != tc.count {
			t.Fatalf("list(%s) = %+v", tc.body, result)
		}
		if tc.count > 0 && (result.Data[0].ID != tc.first || result.Data[tc.count-1].ID != tc.last) {
			t.Fatalf("list(%s) order = %+v", tc.body, result.Data)
		}
	}
	for _, body := range []string{`{"page":0}`, `{"pageSize":-1}`, `{"online":"false"}`, `{`} {
		post(body, viewer.Token, http.StatusBadRequest)
	}
}

func TestRoomPasswordHTTP(t *testing.T) {
	accounts := auth.NewStore()
	owner, err := accounts.SignUp(t.Context(), "host@example.org", "secret", "host")
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := accounts.SignUp(t.Context(), "viewer@example.org", "secret", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	httpapi.Register(mux, accounts, live.NewStore())
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	post := func(path, token string, body any, want int) map[string]any {
		t.Helper()
		res := requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/live/room/"+path, token, body)
		if res.Code != want {
			t.Fatalf("POST %s returned %d, want %d: %v", path, res.Code, want, res.Body)
		}
		return res.Body
	}
	password := map[string]string{"password": " Case 敏感 "}
	room := post("start", owner.Token, password, http.StatusOK)
	if room["isLock"] != true {
		t.Fatalf("start returned isLock = %v", room["isLock"])
	}
	id := itoa(owner.User.ID)
	join := "join/" + id
	post("setting", "", password, http.StatusUnauthorized)
	post("setting", viewer.Token, map[string]any{"room_id": owner.User.ID, "password": ""}, http.StatusNotFound)
	post("setting", owner.Token, json.RawMessage("null"), http.StatusBadRequest)
	updated := post("setting", owner.Token, map[string]any{"subject": "晚场", "password": nil}, http.StatusOK)
	settings, ok := updated["data"].(map[string]any)
	if updated["status"] != "200" || !ok || settings["title"] != "晚场" || settings["isLock"] != true {
		t.Fatalf("title-only settings returned %+v", updated)
	}
	post(join, owner.Token, nil, http.StatusNoContent)
	post(join, viewer.Token, map[string]int{"password": 123}, http.StatusBadRequest)
	post(join, viewer.Token, nil, http.StatusForbidden)
	post(join, viewer.Token, map[string]string{"password": "Case 敏感"}, http.StatusForbidden)
	res := requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/live/room/info/"+id, viewer.Token, nil)
	if res.Code != http.StatusOK || res.Body["member_count"] != float64(0) || res.Body["isLock"] != true {
		t.Fatalf("info after rejected joins = %+v", res)
	}
	post(join, viewer.Token, password, http.StatusNoContent)
	newPassword := map[string]string{"password": "new password"}
	post("setting", owner.Token, newPassword, http.StatusOK)
	post("leave/"+id, viewer.Token, nil, http.StatusNoContent)
	post(join, viewer.Token, password, http.StatusForbidden)
	post(join, viewer.Token, newPassword, http.StatusNoContent)
	post("leave/"+id, viewer.Token, nil, http.StatusNoContent)
	for range live.MaxRoomPasswordAttempts {
		post(join, viewer.Token, nil, http.StatusForbidden)
	}
	post(join, viewer.Token, password, http.StatusTooManyRequests)
	post("setting", owner.Token, map[string]string{"password": ""}, http.StatusOK)
	post(join, viewer.Token, nil, http.StatusNoContent)
	post("start", owner.Token, map[string]string{"password": "changed"}, http.StatusConflict)
	post("leave/"+id, owner.Token, nil, http.StatusNoContent)
	post("setting", owner.Token, password, http.StatusConflict)
	room = post("start", owner.Token, map[string]string{"title": "Public"}, http.StatusOK)
	if room["isLock"] != false {
		t.Fatalf("reopened room isLock = %v", room["isLock"])
	}
	post(join, viewer.Token, nil, http.StatusNoContent)
}
