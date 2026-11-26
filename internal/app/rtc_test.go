package app_test

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/AgoraIO-Community/go-tokenbuilder/accesstoken2"

	"open_live_platform/internal/app"
	"open_live_platform/internal/rtc"
)

func TestRoomRTCTokens(t *testing.T) {
	const appID = "0123456789abcdef0123456789abcdef"
	const cert = "fedcba9876543210fedcba9876543210"
	t.Setenv("AGORA_APP_ID", appID)
	t.Setenv("AGORA_APP_CERTIFICATE", cert)
	t.Setenv("AGORA_TOKEN_EXPIRE", "1800")
	config := app.ConfigFromEnv()
	config.DatabaseURL, config.GiftCatalogFile = "", ""
	a, err := app.New(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	owner, err := a.Accounts.SignUp(t.Context(), "owner@example.test", "secret", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := a.Accounts.SignUp(t.Context(), "viewer@example.test", "secret", "Viewer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.Rooms.Start(t.Context(), owner.User.ID, "Private room", "room-password"); err != nil {
		t.Fatal(err)
	}
	channel := strconv.FormatInt(owner.User.ID, 10)
	join := "/api/live/room/join/" + channel + "?compat=rust"
	renew := "/api/live/token/" + channel + "/"
	request := func(method, path, session, body string, code int) []byte {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+session)
		w := httptest.NewRecorder()
		a.Server.Handler.ServeHTTP(w, req)
		if w.Code != code {
			t.Fatalf("%s %s returned %d, want %d", method, path, w.Code, code)
		}
		return w.Body.Bytes()
	}
	checkToken := func(token rtc.Token, userID int64, publisher bool) {
		t.Helper()
		if token.AppID != appID || token.Channel != channel || token.UID != strconv.FormatInt(userID, 10) ||
			token.IsAnchor != publisher || token.NID == "" || token.NID == "0" || !strings.HasPrefix(token.Token, "007") {
			t.Fatal("incorrect RTC token metadata")
		}
		parsed := accesstoken2.CreateAccessToken()
		if ok, err := parsed.Parse(token.Token); err != nil || !ok {
			t.Fatalf("parse token: %v", err)
		}
		if ok, err := parsed.VerifySignature(cert); err != nil || !ok {
			t.Fatalf("verify signature: %v", err)
		}
		service, ok := parsed.Services[accesstoken2.ServiceTypeRtc].(*accesstoken2.ServiceRtc)
		privileges := map[uint16]uint32{accesstoken2.PrivilegeJoinChannel: 1800}
		if publisher {
			privileges[accesstoken2.PrivilegePublishAudioStream] = 1800
			privileges[accesstoken2.PrivilegePublishVideoStream] = 1800
			privileges[accesstoken2.PrivilegePublishDataStream] = 1800
		}
		if !ok || parsed.AppId != appID || parsed.Expire != 1800 || service.ChannelName != channel ||
			service.Uid != token.NID || !maps.Equal(service.Privileges, privileges) {
			t.Fatal("signed RTC identity, privileges, or TTL differ from the response")
		}
	}
	request(http.MethodGet, renew+"2", viewer.Token, "", http.StatusForbidden)
	for _, tc := range []struct {
		session string
		userID  int64
		role    string
	}{
		{owner.Token, owner.User.ID, "1"},
		{viewer.Token, viewer.User.ID, "2"},
	} {
		var joined struct {
			Data struct {
				RTC rtc.Token `json:"rtcToken"`
			}
		}
		if err := json.Unmarshal(request(http.MethodPost, join, tc.session, `{"password":"room-password"}`, http.StatusOK), &joined); err != nil {
			t.Fatal(err)
		}
		checkToken(joined.Data.RTC, tc.userID, tc.role == "1")
		var renewed struct{ Data rtc.Token }
		if err := json.Unmarshal(request(http.MethodGet, renew+tc.role, tc.session, "", http.StatusOK), &renewed); err != nil {
			t.Fatal(err)
		}
		checkToken(renewed.Data, tc.userID, tc.role == "1")
		if joined.Data.RTC.NID != renewed.Data.NID {
			t.Fatal("renewal changed the RTC user ID")
		}
	}
	request(http.MethodGet, renew+"1", viewer.Token, "", http.StatusForbidden)
	request(http.MethodPost, "/api/live/room/leave/"+channel, viewer.Token, "", http.StatusNoContent)
	request(http.MethodGet, renew+"2", viewer.Token, "", http.StatusForbidden)
}
