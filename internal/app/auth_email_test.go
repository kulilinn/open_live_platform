package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/httpapi"
	"open_live_platform/internal/rtc"
)

type testVerificationSender struct {
	sends int
	code  string
	err   error
}

func (s *testVerificationSender) SendVerificationCode(_ context.Context, _, code string, _ time.Duration) error {
	s.sends++
	s.code = code
	return s.err
}

func TestEmailAuthFlow(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		testEmailAuthFlow(t, app.Config{}, nil)
	})
	t.Run("postgres", func(t *testing.T) {
		config, db := postgresTestDatabase(t)
		testEmailAuthFlow(t, config, func(email string) {
			if _, err := db.Exec(t.Context(), "UPDATE auth_email_codes SET expires_at=now() WHERE email=$1", email); err != nil {
				t.Fatal(err)
			}
		})
	})
}

func testEmailAuthFlow(t *testing.T, config app.Config, expire func(string)) {
	t.Helper()
	config.JWTSecret = "test-jwt-email"
	platform, err := app.New(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	const email = "verified@example.org"
	const joined = "passwordless@example.org"
	const password = "current-password"
	const changed = "new-password"
	var sender testVerificationSender
	verified, err := auth.NewEmailVerification(platform.Accounts, &sender, "test-code-secret", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	httpapi.RegisterWithEmail(mux, platform.Accounts, platform.Rooms, rtc.Signer{}, verified)
	request := func(handler http.Handler, route, token string, fields map[string]any, want int) map[string]any {
		t.Helper()
		return authFlowRequest(t, handler, route, token, fields, want)
	}
	issue := func(email string, signin bool) string {
		t.Helper()
		result := request(mux, "/api/auth/code", "", map[string]any{"email": email, "signin": signin}, http.StatusOK)
		if result["needSend"] != true || len(sender.code) != 4 {
			t.Fatalf("email code response = %+v", result)
		}
		return sender.code
	}
	verify := func(email, code string, status int) map[string]any {
		t.Helper()
		return request(mux, "/api/auth/verifycode", "", map[string]any{"email": email, "code": code}, status)
	}
	reset := func(email, code, status string) {
		t.Helper()
		result := request(mux, "/api/auth/reset", "", map[string]any{"email": email, "code": code, "password": changed}, http.StatusOK)
		if result["status"] != status {
			t.Fatalf("password reset response = %+v, want status %s", result, status)
		}
	}
	request(platform.Server.Handler, "/api/auth/code", "", nil, http.StatusServiceUnavailable)
	request(platform.Server.Handler, "/api/auth/verifycode", "", nil, http.StatusServiceUnavailable)
	request(platform.Server.Handler, "/api/auth/reset", "", nil, http.StatusServiceUnavailable)
	credentials := map[string]any{"email": email, "password": password, "account": "Verified User"}
	request(mux, "/api/auth/signup", "", credentials, http.StatusUnauthorized)
	if result := request(mux, "/api/auth/signup?compat=rust", "", credentials, http.StatusOK); result["status"] != "4013" {
		t.Fatalf("Rust signup without proof = %+v", result)
	}
	request(mux, "/api/auth/code", "", map[string]any{"email": email, "signin": false, "sendMail": false, "code": "1111"}, http.StatusBadRequest)
	if sender.sends != 0 {
		t.Fatal("caller-supplied code reached the sender")
	}
	sender.err = errors.New("mail transport failed")
	request(mux, "/api/auth/code", "", map[string]any{"email": email, "signin": false}, http.StatusServiceUnavailable)
	sender.err = nil
	registration := issue(strings.ToUpper(email), false)
	if result := request(mux, "/api/auth/code", "", map[string]any{"email": email, "signin": false}, http.StatusOK); result["needSend"] != false || sender.sends != 2 {
		t.Fatalf("duplicate request resent the code: %+v, total sends = %d", result, sender.sends)
	}
	verify(joined, registration, http.StatusUnauthorized)
	reset(email, registration, "4013")
	credentials["code"] = registration
	if result := request(mux, "/api/auth/signup?compat=rust", "", credentials, http.StatusOK); result["status"] != "200" {
		t.Fatalf("verified signup response = %+v", result)
	}
	verify(email, registration, http.StatusUnauthorized)
	if _, err := platform.Accounts.SignIn(t.Context(), email, password); err != nil {
		t.Fatalf("verified account cannot authenticate with its password: %v", err)
	}
	request(mux, "/api/auth/code", "", map[string]any{"email": joined, "signin": true}, http.StatusNotFound)
	request(mux, "/api/auth/code", "", map[string]any{"email": email, "signin": false}, http.StatusConflict)

	loginCode := issue(joined, false)
	token := verify(joined, loginCode, http.StatusOK)["token"].(string)
	if session, ok, err := platform.Accounts.Refresh(t.Context(), token); err != nil || !ok || session.User.Email != joined {
		t.Fatalf("passwordless refresh returned %t, email %q, error %v", ok, session.User.Email, err)
	}
	verify(joined, loginCode, http.StatusUnauthorized)
	request(mux, "/api/auth/signup", "", map[string]any{"email": joined, "password": password, "account": "Other", "code": loginCode}, http.StatusUnauthorized)

	resetCode := issue(email, true)
	reset(joined, resetCode, "4013")
	reset(email, resetCode, "200")
	reset(email, resetCode, "4013")
	if _, err := platform.Accounts.SignIn(t.Context(), email, password); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("old password accepted after reset: %v", err)
	}
	if _, err := platform.Accounts.SignIn(t.Context(), email, changed); err != nil {
		t.Fatal(err)
	}
	if expire != nil {
		code := issue(email, true)
		expire(email)
		verify(email, code, http.StatusUnauthorized)
	}
}

func authFlowRequest(t *testing.T, handler http.Handler, route, token string, fields map[string]any, want int) map[string]any {
	t.Helper()
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, route, strings.NewReader(string(body)))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != want {
		t.Fatalf("%s returned HTTP %d, want %d: %s", route, w.Code, want, w.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}
