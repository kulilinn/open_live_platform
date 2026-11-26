package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/httpapi"
	"open_live_platform/internal/live"
)

func TestUserProfileHTTP(t *testing.T) {
	accounts, rooms := auth.NewStore(), live.NewStore()
	owner, err := accounts.SignUp(t.Context(), "profile-owner@example.com", "secret", "Profile Owner")
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := accounts.SignUp(t.Context(), "profile-viewer@example.com", "secret", "Profile Viewer")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	httpapi.Register(mux, accounts, rooms)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	me := requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/user/me", owner.Token, nil)
	meData := me.Body["data"].(map[string]any)
	if me.Code != http.StatusOK || me.Body["status"] != "200" ||
		meData["id"] != itoa(owner.User.ID) || meData["displayName"] != "Profile Owner" {
		t.Fatalf("me = %d %+v", me.Code, me.Body)
	}
	other := requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/user/"+itoa(viewer.User.ID), owner.Token, nil)
	otherData := other.Body["data"].(map[string]any)
	if other.Code != http.StatusOK || otherData["id"] != itoa(viewer.User.ID) ||
		otherData["displayName"] != "Profile Viewer" {
		t.Fatalf("other = %d %+v", other.Code, other.Body)
	}
	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/user/set_language", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+owner.Token)
	req.Header.Set("x-loverose-lang", "en")
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var language map[string]any
	if err := json.NewDecoder(res.Body).Decode(&language); err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK || language["status"] != "200" {
		t.Fatalf("set language = %d %+v", res.StatusCode, language)
	}
	me = requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/user/me", owner.Token, nil)
	if me.Body["data"].(map[string]any)["language"] != "en" {
		t.Fatalf("language profile = %+v", me.Body)
	}
}
