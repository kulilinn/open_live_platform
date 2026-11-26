package httpapi_test

import (
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
}
