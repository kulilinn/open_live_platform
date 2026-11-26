package app_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/dm"
	"open_live_platform/internal/httpapi"
	"open_live_platform/internal/live"
	"open_live_platform/internal/relay"
	"open_live_platform/internal/rtc"
	"open_live_platform/internal/store/postgres"
)

func TestPostgresModerationControlsAuthenticationAndMessages(t *testing.T) {
	config, sqlDB := postgresTestDatabase(t)
	config.JWTSecret = "moderation-fixture-secret"
	plat, err := app.New(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer plat.Close()
	target, err := postgres.Open(t.Context(), config.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Shutdown()
	server := httptest.NewServer(plat.Server.Handler)
	defer server.Close()

	const phc = "$argon2id$v=19$m=19456,t=2,p=1$m2q/aRQ4nPk0jnUOVXAKIw$tmnbwHvtGHyoSsVq1KvkG2dQGM6SOEPL1QY/Xf18NEo"
	const email, password = "legacy-moderation@example.org", "12345678"
	const userID int64 = 78591199061446657
	source := postgres.LegacyAccount{ID: userID, Email: email, LoginEmail: email, Account: "Viewer", Password: phc,
		Status: auth.AccountMuted, LimitUntil: time.Now().Add(time.Hour).Unix(), ViolationID: 89}
	record := postgres.LegacyViolationRecord{ID: 89, UserID: userID, Reason: "chat misuse,spam", LimitDuration: 3600,
		CreatedAt: time.Now(), CreatedBy: "test review", Type: 2}
	importAccount := func() {
		t.Helper()
		_, err := target.ImportLegacyAccountSnapshot(t.Context(), func(yield func(postgres.LegacyAccount, error) bool) {
			yield(source, nil)
		}, func(yield func(postgres.LegacyViolationRecord, error) bool) {
			yield(record, nil)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	importAccount()
	anchor := subscriptionTestUser(t, plat, "moderation-anchor", true)
	if err := plat.Rooms.Join(t.Context(), anchor.User.ID, userID, ""); err != nil {
		t.Fatal(err)
	}
	credentials := map[string]any{"email": email, "password": password}
	login := authFlowRequest(t, plat.Server.Handler, "/api/auth/signin?compat=rust", "", credentials, http.StatusOK)
	if login["status"] != "200" {
		t.Fatalf("mute blocked login: %+v", login)
	}
	refresh := login["token"].(string)
	jwt, ok, err := plat.Accounts.Refresh(t.Context(), refresh)
	if err != nil || !ok {
		t.Fatalf("refresh for muted account = %t, %v", ok, err)
	}
	opaque, err := plat.Accounts.SignIn(t.Context(), email, password)
	if err != nil {
		t.Fatal(err)
	}
	status := authFlowRequest(t, plat.Server.Handler, "/api/user/check_user_status", opaque.Token, nil, http.StatusOK)["data"].(map[string]any)
	if status["ty"] != float64(auth.AccountMuted) || status["allow"] != false || status["limit_total"] != float64(3600) || len(status["reason"].([]any)) != 2 {
		t.Fatalf("status = %+v", status)
	}
	chatInput := map[string]any{"room_id": anchor.User.ID, "text": "muted"}
	authFlowRequest(t, plat.Server.Handler, "/api/live/room/chat", opaque.Token, chatInput, http.StatusForbidden)
	if err := plat.Rooms.Send(t.Context(), anchor.User.ID, userID, "muted"); !errors.Is(err, live.ErrMuted) {
		t.Fatalf("muted user wrote direct room chat: %v", err)
	}
	dmID, err := plat.DM.SetDMSubscription(t.Context(), dm.Metadata{Fan: dm.User{ID: userID}, Anchor: dm.User{ID: anchor.User.ID, Anchor: true}}, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plat.DM.SendDM(t.Context(), dm.SendInput{ConversationID: dmID, SenderID: userID, ReceiverID: anchor.User.ID, Type: 1, Content: "muted"}); !errors.Is(err, dm.ErrForbidden) {
		t.Fatalf("muted user wrote direct DM: %v", err)
	}
	address := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/connector/" + jwt.Token + "/" + relay.Signature(jwt.Token, config.RelaySecret) +
		"?room_id=" + strconv.FormatInt(anchor.User.ID, 10)
	conn, response, err := websocket.DefaultDialer.DialContext(t.Context(), address, nil)
	if err != nil {
		if response != nil {
			response.Body.Close()
		}
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := conn.WriteJSON(map[string]string{"type": "chat", "text": "muted"}); err != nil {
		t.Fatal(err)
	}
	var reply map[string]any
	if err := conn.ReadJSON(&reply); err != nil || reply["type"] != "error" {
		t.Fatalf("muted relay response = %+v, error = %v", reply, err)
	}
	if _, err := sqlDB.Exec(t.Context(), `UPDATE users SET account_limit_until=1 WHERE id=$1`, userID); err != nil {
		t.Fatal(err)
	}
	if err := plat.Rooms.Send(t.Context(), anchor.User.ID, userID, "after expiry"); err != nil {
		t.Fatalf("expired moderation still blocked sending: %v", err)
	}
	if err := conn.ReadJSON(&reply); err != nil || reply["type"] != "chat" {
		t.Fatalf("after expiry relay = %+v, error = %v", reply, err)
	}
	code := authFlowRequest(t, plat.Server.Handler, "/api/auth/exchange_code", jwt.Token, nil, http.StatusOK)["data"].(map[string]any)["code"].(string)
	if _, err := sqlDB.Exec(t.Context(), `UPDATE users SET account_status=2,account_limit_until=-1 WHERE id=$1`, userID); err != nil {
		t.Fatal(err)
	}
	if result := authFlowRequest(t, plat.Server.Handler, "/api/auth/exchange_token", "", map[string]any{"code": code}, http.StatusOK); result["status"] != "4047" {
		t.Fatalf("disabled account exchanged a token: %+v", result)
	}
	if result := authFlowRequest(t, plat.Server.Handler, "/api/auth/signin?compat=rust", "", credentials, http.StatusOK); result["status"] != "4047" || result["token"] != nil {
		t.Fatalf("disabled compatibility login = %+v", result)
	}
	authFlowRequest(t, plat.Server.Handler, "/api/auth/signin", "", credentials, http.StatusForbidden)
	sender := &testVerificationSender{}
	emailVerification, err := auth.NewEmailVerification(plat.Accounts, sender, "email-code-secret", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	mailMux := http.NewServeMux()
	httpapi.RegisterWithEmail(mailMux, plat.Accounts, plat.Rooms, rtc.Signer{}, emailVerification)
	authFlowRequest(t, mailMux, "/api/auth/code", "", map[string]any{"email": email, "signin": true}, http.StatusOK)
	if result := authFlowRequest(t, mailMux, "/api/auth/verifycode", "", map[string]any{"email": email, "code": sender.code}, http.StatusOK); result["status"] != "4047" {
		t.Fatalf("disabled account signed in via email: %+v", result)
	}
	_, response, err = websocket.DefaultDialer.DialContext(t.Context(), address, nil)
	if response != nil {
		response.Body.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("disabled account reconnected WebSocket: %v", err)
	}
	source.Status, source.LimitUntil = auth.AccountBanned, -1
	importAccount()
	if _, _, err := conn.ReadMessage(); err == nil || !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.ClosePolicyViolation) {
		t.Fatalf("moderation sync did not close pre-existing relay: %v", err)
	}
	if _, ok, err := plat.Accounts.Lookup(t.Context(), opaque.Token); err != nil || ok {
		t.Fatalf("moderation sync did not revoke opaque session: %t, %v", ok, err)
	}
}
