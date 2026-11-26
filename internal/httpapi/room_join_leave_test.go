package httpapi_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/httpapi"
	"open_live_platform/internal/live"
)

func TestRustJoinLeaveResponse(t *testing.T) {
	accounts, rooms := auth.NewStore(), live.NewStore()
	owner, err := accounts.SignUp(t.Context(), "join-owner@example.com", "secret", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := accounts.SignUp(t.Context(), "join-viewer@example.com", "secret", "Viewer")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	httpapi.Register(mux, accounts, rooms)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	start := requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/live/room/start", owner.Token, map[string]string{"subject": "Join"})
	roomID := itoa(int64(start.Body["id"].(float64)))
	room, err := rooms.Info(t.Context(), owner.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	info := start.Body["data"].(map[string]any)
	if start.Code != http.StatusOK || info["anchorNid"] != float64(live.DefaultAnchorNID) ||
		info["liveId"] != itoa(room.Session.LiveID) || info["liveId"] == roomID {
		t.Fatalf("Rust start = %d %+v", start.Code, start.Body)
	}
	rustJoin := rustRequest(t, server.Client(), http.MethodPost, server.URL+"/api/live/room/join/"+roomID, viewer.Token, map[string]string{}, http.StatusOK)
	data := rustJoin["data"].(map[string]any)
	if rustJoin["status"] != "200" || data["room"] == nil || data["languages"] == nil ||
		data["user"].(map[string]any)["id"] != float64(viewer.User.ID) ||
		data["startTime"] != float64(room.Session.StartedAt.Unix()) ||
		data["room"].(map[string]any)["liveId"] != info["liveId"] {
		t.Fatalf("Rust join = %+v", rustJoin)
	}
	rtc := data["rtcToken"].(map[string]any)
	if rtc["channel"] != roomID || rtc["token"] != "" {
		t.Fatalf("Rust RTC token = %+v", rtc)
	}
	ownerJoin := rustRequest(t, server.Client(), http.MethodPost, server.URL+"/api/live/room/join/"+roomID, owner.Token, nil, http.StatusOK)
	ownerRTC := ownerJoin["data"].(map[string]any)["rtcToken"].(map[string]any)
	if ownerRTC["nid"] != itoa(int64(live.DefaultAnchorNID)) || ownerRTC["isAnchor"] != true {
		t.Fatalf("host RTC fields = %+v", ownerRTC)
	}
	rustLeave := rustRequest(t, server.Client(), http.MethodPost, server.URL+"/api/live/room/leave/"+roomID, viewer.Token, nil, http.StatusOK)
	if rustLeave["status"] != "200" {
		t.Fatalf("Rust leave = %+v", rustLeave)
	}
	legacyJoin := requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/live/room/join/"+roomID, viewer.Token, nil)
	if legacyJoin.Code != http.StatusNoContent {
		t.Fatalf("legacy join = %d", legacyJoin.Code)
	}
}

func rustRequest(t *testing.T, client *http.Client, method, url, token string, body any, want int) map[string]any {
	t.Helper()
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, url+"?compat=rust", bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.livemedia+json")
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var result map[string]any
	if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != want {
		t.Fatalf("Rust request status = %d body=%+v", res.StatusCode, result)
	}
	return result
}
