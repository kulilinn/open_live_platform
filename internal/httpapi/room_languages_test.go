package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/httpapi"
	"open_live_platform/internal/live"
)

func TestJoinedRoomTranslationLanguages(t *testing.T) {
	accounts, rooms := auth.NewStore(), live.NewStore()
	signUp := func(account string) auth.Session {
		t.Helper()
		session, err := accounts.SignUp(t.Context(), account+"@example.test", "test-password", account)
		if err != nil {
			t.Fatal(err)
		}
		return session
	}
	owner, first, second := signUp("owner"), signUp("first"), signUp("second")
	mux := http.NewServeMux()
	httpapi.Register(mux, accounts, rooms)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	const base = "/api/live/room/"
	roomID := itoa(owner.User.ID)
	setLanguage := func(session auth.Session, lang string) {
		t.Helper()
		res := requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/user/set_info", session.Token, map[string]string{"transLang": lang})
		if res.Code != http.StatusOK || res.Body["status"] != "200" {
			t.Fatalf("set_info = %+v", res)
		}
	}
	post := func(path string, session auth.Session, body any, want int) {
		t.Helper()
		res := requestJSON(t, server.Client(), http.MethodPost, server.URL+base+path, session.Token, body)
		if res.Code != want {
			t.Fatalf("%s status = %d, want %d", path, res.Code, want)
		}
	}
	join := func(session auth.Session, want ...any) {
		t.Helper()
		response := rustRequest(t, server.Client(), http.MethodPost, server.URL+base+"join/"+roomID, session.Token, nil, http.StatusOK)
		data := response["data"].(map[string]any)
		got := data["languages"]
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("join by %s languages = %v, want %v", session.User.Account, got, want)
		}
		room := data["room"].(map[string]any)
		if room["memberCount"] != float64(len(room["members"].([]any))) {
			t.Fatal("join count differs from its member snapshot")
		}
	}

	setLanguage(owner, "es")
	post("start", owner, map[string]string{"subject": "Room", "transLang": "fr"}, http.StatusOK)
	join(first, "en")
	setLanguage(first, "ja")
	setLanguage(second, "ja")
	post("join/"+roomID, second, nil, http.StatusNoContent)
	join(first, "ja")
	join(owner, "fr", "ja")
	post("leave/"+roomID, first, nil, http.StatusNoContent)
	join(owner, "fr", "ja")
	post("kicked/"+roomID+"/"+itoa(second.User.ID), owner, nil, http.StatusNoContent)
	join(owner, "fr")

	post("setting", owner, map[string]string{"transLang": "ko"}, http.StatusOK)
	join(owner, "ko")
	post("leave/"+roomID, owner, nil, http.StatusNoContent)
	post("start", owner, map[string]string{"subject": "Next session"}, http.StatusOK)
	setLanguage(second, "")
	join(second, "en")
	join(owner, "en", "ko")
}
