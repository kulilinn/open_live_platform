package app_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
)

func TestTrustedInternalAuth(t *testing.T) {
	t.Run("memory", func(t *testing.T) { testTrustedInternalAuth(t, app.Config{}, nil) })
	t.Run("postgres", func(t *testing.T) {
		config, db := postgresTestDatabase(t)
		testTrustedInternalAuth(t, config, db)
	})
}

func testTrustedInternalAuth(t *testing.T, config app.Config, db *pgx.Conn) {
	t.Helper()
	const callback = "https://client.example.test/callback?lang=ja"
	const otherCallback = "https://client.example.test/other"
	const clientA, clientB, service = "client-a-secret", "client-b-secret", "partner-service-token"
	hash := func(raw string) string {
		sum := sha256.Sum256([]byte(raw))
		return hex.EncodeToString(sum[:])
	}
	config.TrustedOAuthClientsFile = filepath.Join(t.TempDir(), "oauth-clients.json")
	clients, err := json.Marshal([]map[string]any{
		{"client_id": "client-a", "token_hash": hash(clientA), "redirect_uris": []string{callback, otherCallback}},
		{"client_id": "client-b", "token_hash": hash(clientB), "redirect_uris": []string{callback}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.TrustedOAuthClientsFile, clients, 0600); err != nil {
		t.Fatal(err)
	}
	config.JWTSecret, config.InternalUserToken = "private-local-fixture", service
	config.Google = auth.GoogleConfig{ClientID: "real-audience", Client: &http.Client{Transport: googleTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "www.googleapis.com" || r.URL.Path != "/oauth2/v2/tokeninfo" || r.URL.Query().Get("access_token") != "g-proof" {
			return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{
			"user_id":"provider-user", "email":"sign-in-test@gmail.com","verified_email":true,
			"expires_in":60,"audience":"real-audience"
		}`))}, nil
	})}}
	first, err := app.New(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(first.Close)
	second := first
	if db != nil {
		second, err = app.New(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(second.Close)
	}
	ctx := t.Context()
	user := subscriptionTestUser(t, first, "trusted-user", false)
	foreign := subscriptionTestUser(t, first, "trusted-foreign", false)
	uid := strconv.FormatInt(user.User.ID, 10)
	userAccess := func(password string) string {
		t.Helper()
		login, err := first.Accounts.(*auth.JWTAccounts).SignInJWT(ctx, user.User.Email, password)
		if err != nil {
			t.Fatal(err)
		}
		access, ok, err := first.Accounts.Refresh(ctx, login.Token)
		if !ok || err != nil {
			t.Fatalf("get access: ok=%v err=%v", ok, err)
		}
		return access.Token
	}
	access := userAccess("password")
	call := func(platform *app.App, method, route, token, service string, body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(ctx, method, route, bytes.NewReader(body))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		r.Header.Set("X-Inner-Service", service)
		w := httptest.NewRecorder()
		platform.Server.Handler.ServeHTTP(w, r)
		return w
	}
	expect := func(w *httptest.ResponseRecorder, code int) {
		t.Helper()
		if w.Code != code {
			t.Fatalf("response HTTP %d, want %d: %s", w.Code, code, w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("shared identity response was cacheable")
		}
	}
	body := func(v any) []byte {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	asObject := func(w *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		var result map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"jwt_key", "relay_token", "password", "family_id", "private-local-fixture"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatalf("user response exposed a private field")
			}
		}
		return result
	}
	const state = "a b&expected=+界"
	authorize := func(client, redirect, token, actor string, statusCode int) string {
		t.Helper()
		q := url.Values{"client_id": {client}, "user_id": {actor}, "redirect_uri": {redirect}, "response_type": {"code"}, "state": {state}}
		w := call(first, http.MethodGet, "/api/auth_internal/get_auth_code?"+q.Encode(), token, "", nil)
		expect(w, statusCode)
		if statusCode != http.StatusOK {
			return ""
		}
		var link string
		if err := json.Unmarshal(w.Body.Bytes(), &link); err != nil {
			t.Fatal(err)
		}
		result, err := url.Parse(link)
		if err != nil || result.Host != "client.example.test" || result.Query().Get("state") != state ||
			result.Query().Get("user_id") != actor || result.Query().Get("lang") != "ja" || len(result.Query().Get("code")) != 32 {
			t.Fatal("invalid authorization callback")
		}
		return result.Query().Get("code")
	}
	redeem := func(client, redirect, token, code, actor string, statusCode int) {
		t.Helper()
		input := body(map[string]string{"client_id": client, "user_id": actor, "redirect_uri": redirect, "auth_code": code})
		w := call(second, http.MethodPost, "/api/auth_internal/check_auth_code", token, "", input)
		expect(w, statusCode)
		if statusCode == http.StatusOK && asObject(w)["id"] != float64(user.User.ID) {
			t.Fatal("grant returned another account")
		}
	}
	authorize("client-a", callback, "", uid, http.StatusUnauthorized)
	authorize("client-a", callback, access, strconv.FormatInt(foreign.User.ID, 10), http.StatusUnauthorized)
	authorize("client-a", "https://evil.example.test/callback", access, uid, http.StatusBadRequest)
	authorize("client-a", callback+"&other=1", access, uid, http.StatusBadRequest)
	code := authorize("client-a", callback, access, uid, http.StatusOK)
	redeem("client-a", callback, "wrong-client-token", code, uid, http.StatusForbidden)
	redeem("client-b", callback, clientB, code, uid, http.StatusUnauthorized)
	redeem("client-a", otherCallback, clientA, code, uid, http.StatusUnauthorized)
	redeem("client-a", "", clientA, code, uid, http.StatusUnauthorized)
	redeem("client-a", callback, clientA, code, strconv.FormatInt(foreign.User.ID, 10), http.StatusUnauthorized)

	payload := body(map[string]string{"client_id": "client-a", "user_id": uid, "redirect_uri": callback, "auth_code": code})
	responses := make(chan *httptest.ResponseRecorder, 2)
	for _, platform := range []*app.App{first, second} {
		go func() {
			responses <- call(platform, http.MethodPost, "/api/auth_internal/check_auth_code", clientA, "", payload)
		}()
	}
	var successful int
	for range 2 {
		w := <-responses
		if w.Code == http.StatusOK {
			successful++
			if asObject(w)["id"] != float64(user.User.ID) {
				t.Fatal("grant returned another user")
			}
		} else {
			expect(w, http.StatusUnauthorized)
		}
	}
	if successful != 1 {
		t.Fatalf("same code accepted %d times", successful)
	}
	if db != nil {
		code := authorize("client-a", callback, access, uid, http.StatusOK)
		sum := sha256.Sum256([]byte(code))
		if _, err := db.Exec(ctx, `UPDATE trusted_auth_grants SET expires_at=now()-interval '1 second' WHERE code_digest=$1`, sum[:]); err != nil {
			t.Fatal(err)
		}
		redeem("client-a", callback, clientA, code, uid, http.StatusUnauthorized)
	}
	inferred := authorize("client-b", callback, access, uid, http.StatusOK)
	redeem("client-b", "", clientB, inferred, uid, http.StatusOK)
	revoke := authorize("client-b", callback, access, uid, http.StatusOK)
	if err := first.Accounts.Revoke(ctx, access); err != nil {
		t.Fatal(err)
	}
	redeem("client-b", "", clientB, revoke, uid, http.StatusUnauthorized)

	const userPath = "/api/microservices/user/"
	expect(call(second, http.MethodGet, userPath+uid, "", auth.TrustedUserService, nil), http.StatusForbidden)
	expect(call(second, http.MethodGet, userPath+uid, service, "other", nil), http.StatusForbidden)
	expect(call(second, http.MethodGet, userPath+"invalid", service, auth.TrustedUserService, nil), http.StatusBadRequest)
	got := call(second, http.MethodGet, userPath+uid, service, auth.TrustedUserService, nil)
	expect(got, http.StatusOK)
	if asObject(got)["data"].(map[string]any)["id"] != float64(user.User.ID) {
		t.Fatal("trusted user directory could not read account")
	}
	post := func(route string, v any, want int) map[string]any {
		t.Helper()
		w := call(second, http.MethodPost, userPath+route, service, auth.TrustedUserService, body(v))
		expect(w, want)
		return asObject(w)
	}
	post("verify_password", map[string]string{"email": user.User.Email, "password": "wrong"}, http.StatusUnauthorized)
	post("verify_password", map[string]string{"email": user.User.Email, "password": "password"}, http.StatusOK)
	post("change_password", map[string]string{"email": user.User.Email, "oldPassword": "bad-pass", "newPassword": "new-password"}, http.StatusUnauthorized)
	post("change_password", map[string]string{"email": user.User.Email, "oldPassword": "password", "newPassword": "new-password"}, http.StatusOK)
	post("verify_password", map[string]string{"email": user.User.Email, "password": "password"}, http.StatusUnauthorized)
	access = userAccess("new-password")
	oauthOnly := authorize("client-a", callback, access, uid, http.StatusOK)
	post("verify_code", map[string]string{"code": oauthOnly}, http.StatusUnauthorized)
	redeem("client-a", callback, clientA, oauthOnly, uid, http.StatusOK)
	post("verify_google", map[string]string{"email": "sign-in-test@gmail.com"}, http.StatusUnauthorized)
	post("verify_google", map[string]string{"email": user.User.Email, "token": "g-proof"}, http.StatusUnauthorized)
	verified := post("verify_google", map[string]string{"email": "sign-in-test@gmail.com", "token": "g-proof"}, http.StatusOK)
	if verified["isNew"] != true || verified["user"].(map[string]any)["email"] != "sign-in-test@gmail.com" {
		t.Fatal("verified Google proof did not resolve the subject")
	}
	post("exchange_code", map[string]string{"userId": uid}, http.StatusUnauthorized)
	post("exchange_code", map[string]string{"userId": strconv.FormatInt(foreign.User.ID, 10), "accessToken": access}, http.StatusUnauthorized)
	mic := post("exchange_code", map[string]string{"userId": uid, "accessToken": access}, http.StatusOK)["data"].(map[string]any)["code"].(string)
	redeem("client-a", callback, clientA, mic, uid, http.StatusUnauthorized)
	if view := post("verify_code", map[string]string{"code": mic}, http.StatusOK)["data"].(map[string]any); view["id"] != float64(user.User.ID) {
		t.Fatal("microservice exchange returned another user")
	}
	post("verify_code", map[string]string{"code": mic}, http.StatusUnauthorized)
}
