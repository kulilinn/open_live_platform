package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/httpapi"
	"open_live_platform/internal/live"
)

func TestRoomStatusHTTP(t *testing.T) {
	accounts, rooms := auth.NewStore(), live.NewStore()
	owner, err := accounts.SignUp(t.Context(), "status-owner@example.com", "secret", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := accounts.SignUp(t.Context(), "status-viewer@example.com", "secret", "Viewer")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	httpapi.Register(mux, accounts, rooms)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/live/room/start", owner.Token, map[string]string{"title": "Status"})
	roomID := itoa(owner.User.ID)
	requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/live/room/join/"+roomID, viewer.Token, nil)

	kicked := requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/live/room/kicked/"+roomID+"/"+itoa(viewer.User.ID), viewer.Token, nil)
	if kicked.Code != http.StatusOK || kicked.Body["status"] != "200" || kicked.Body["data"] != false {
		t.Fatalf("kicked = %d %+v", kicked.Code, kicked.Body)
	}
	banned := requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/live/room/banned/"+roomID+"/"+itoa(viewer.User.ID), viewer.Token, nil)
	if banned.Code != http.StatusOK || banned.Body["data"] != false {
		t.Fatalf("banned = %d %+v", banned.Code, banned.Body)
	}
	limited := requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/live/room/limited/"+roomID, viewer.Token, nil)
	limitData := limited.Body["data"].(map[string]any)
	if limited.Code != http.StatusOK || limitData["limit_allow"] != true || limitData["limit_ty"] != float64(0) {
		t.Fatalf("limited = %d %+v", limited.Code, limited.Body)
	}
	password := requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/live/room/check_password/"+roomID, viewer.Token, nil)
	if password.Code != http.StatusOK || password.Body["data"].(map[string]any)["isLock"] != false {
		t.Fatalf("check password = %d %+v", password.Code, password.Body)
	}
	online := requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/live/room/online", owner.Token, nil)
	if online.Code != http.StatusOK || online.Body["data"].(map[string]any)["online"] != float64(1) {
		t.Fatalf("online = %d %+v", online.Code, online.Body)
	}

	audience := requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/live/room/audience/"+roomID+"/"+itoa(viewer.User.ID), viewer.Token, nil)
	if audience.Code != http.StatusOK || audience.Body["data"].(map[string]any)["id"] != itoa(viewer.User.ID) {
		t.Fatalf("audience = %d %+v", audience.Code, audience.Body)
	}
	audiences := requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/live/room/audiences/"+roomID, viewer.Token, nil)
	if audiences.Code != http.StatusOK || len(audiences.Body["data"].([]any)) != 1 {
		t.Fatalf("audiences = %d %+v", audiences.Code, audiences.Body)
	}
	paged := requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/live/room/audiences/"+roomID, viewer.Token, map[string]any{})
	if paged.Code != http.StatusOK || paged.Body["page"] != float64(0) || paged.Body["members"] != float64(1) {
		t.Fatalf("paged audiences = %d %+v", paged.Code, paged.Body)
	}

	requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/live/room/add_admin/"+roomID+"/"+itoa(viewer.User.ID), owner.Token, nil)
	admins := requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/live/room/admins/"+roomID, viewer.Token, nil)
	if admins.Code != http.StatusOK || len(admins.Body["data"].([]any)) != 1 {
		t.Fatalf("admins = %d %+v", admins.Code, admins.Body)
	}
	requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/live/room/ban/"+roomID+"/"+itoa(viewer.User.ID), owner.Token, nil)
	banned = requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/live/room/banned/"+roomID+"/"+itoa(viewer.User.ID), owner.Token, nil)
	if banned.Body["data"] != true {
		t.Fatalf("banned after mute = %+v", banned.Body)
	}
	bans := requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/live/room/bans/"+roomID, owner.Token, nil)
	if bans.Code != http.StatusOK || len(bans.Body["data"].([]any)) != 1 {
		t.Fatalf("bans = %d %+v", bans.Code, bans.Body)
	}
	requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/live/room/kicked/"+roomID+"/"+itoa(viewer.User.ID), owner.Token, nil)
	kicked = requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/live/room/kicked/"+roomID+"/"+itoa(viewer.User.ID), owner.Token, nil)
	if kicked.Body["data"] != true {
		t.Fatalf("kicked after kick = %+v", kicked.Body)
	}

	missing := requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/live/room/kicked/999/999", owner.Token, nil)
	if missing.Code != http.StatusOK || missing.Body["status"] != "200" {
		t.Fatalf("missing status = %d %+v", missing.Code, missing.Body)
	}
}
