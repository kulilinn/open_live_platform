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
	status := requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/user/check_user_status", owner.Token, nil)
	statusData := status.Body["data"].(map[string]any)
	if status.Code != http.StatusOK || status.Body["status"] != "200" ||
		statusData["allow"] != true || statusData["ty"] != float64(0) {
		t.Fatalf("check status = %d %+v", status.Code, status.Body)
	}
	apps := requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/user/apps", owner.Token, map[string]any{})
	if apps.Code != http.StatusOK || apps.Body["status"] != "200" || len(apps.Body["data"].([]any)) != 0 {
		t.Fatalf("user apps = %d %+v", apps.Code, apps.Body)
	}
	password := requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/auth/changepassword", owner.Token, map[string]string{
		"old_password": "secret", "new_password": "new-secret",
	})
	if password.Code != http.StatusOK || password.Body["status"] != "200" {
		t.Fatalf("change password = %d %+v", password.Code, password.Body)
	}
	signin := requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/auth/signin", "", map[string]string{
		"email": owner.User.Email, "password": "new-secret", "account": "ignored",
	})
	if signin.Code != http.StatusOK {
		t.Fatalf("signin after password change = %d %+v", signin.Code, signin.Body)
	}
}

func TestSetUserTranslationLanguageHTTP(t *testing.T) {
	accounts := auth.NewStore()
	viewer, err := accounts.SignUp(t.Context(), "translation-viewer@example.com", "secret", "Viewer")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	httpapi.Register(mux, accounts, live.NewStore())
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	updates := []struct {
		body map[string]any
		want string
	}{
		{body: map[string]any{"transLang": "fr"}, want: "fr"},
		{body: map[string]any{"trans": true}, want: "fr"},
		{body: map[string]any{"transLang": nil}, want: "fr"},
		{body: map[string]any{"transLang": ""}, want: ""},
	}
	for _, update := range updates {
		set := requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/user/set_info", viewer.Token, update.body)
		if set.Code != http.StatusOK || set.Body["status"] != "200" || len(set.Body) != 1 {
			t.Fatalf("set_info %v = %d %+v", update.body, set.Code, set.Body)
		}
		me := requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/user/me", viewer.Token, nil)
		profile := me.Body["data"].(map[string]any)
		if profile["transLang"] != update.want || profile["trans"] != false {
			t.Fatalf("profile after set_info %v = %+v", update.body, profile)
		}
	}
}
