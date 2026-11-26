package app_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
)

type googleTransport func(*http.Request) (*http.Response, error)

func (f googleTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGoogleLogin(t *testing.T) {
	t.Run("memory", func(t *testing.T) { testGoogleLogin(t, app.Config{}) })
	t.Run("postgres", func(t *testing.T) { cfg, _ := postgresTestDatabase(t); testGoogleLogin(t, cfg) })
}

func testGoogleLogin(t *testing.T, cfg app.Config) {
	cfg.JWTSecret = "local-fixture-secret"
	var users atomic.Int64
	cfg.Google = auth.GoogleConfig{ClientID: "fixture-client", Client: &http.Client{Transport: googleTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "www.googleapis.com" {
			t.Fatalf("unexpected host %v", r.URL.Host)
		}
		var data map[string]any
		switch r.URL.Path {
		case "/oauth2/v2/tokeninfo":
			raw := r.URL.Query().Get("access_token")
			data = map[string]any{"verified_email": true, "user_id": "google-id", "audience": "fixture-client", "email": "Example@gmail.com", "expires_in": 250}
			switch raw {
			case "other-app":
				data["audience"] = "unrelated-client"
			case "unverified":
				data["verified_email"] = false
			case "expired":
				data["expires_in"] = 0
			case "third", "workspace":
				data["email"] = raw + "@example.org"
				data["user_id"] = raw
			case "changed-email":
				data["email"] = "renamed@gmail.com"
			case "second-subject":
				data["user_id"] = "another"
			case "existing":
				data["email"] = "registered@gmail.com"
				data["user_id"] = "known"
			case "normal":
			default:
				return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader(""))}, nil
			}
		case "/oauth2/v2/userinfo":
			users.Add(1)
			raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			data = map[string]any{"id": raw, "email": raw + "@example.org"}
			if raw == "workspace" {
				data["hd"] = "example.org"
			}
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		body, _ := json.Marshal(data)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})}}
	open := func() *app.App {
		t.Helper()
		a, err := app.New(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(a.Close)
		return a
	}
	a := open()
	post := func(token string, status int) map[string]any {
		t.Helper()
		return authFlowRequest(t, a.Server.Handler, "/api/auth/verifygoogle", "", map[string]any{"token": token}, status)
	}
	for _, token := range []string{"other-app", "unverified", "expired", "invalid"} {
		post(token, 401)
	}
	post("third", 403)
	first := post("normal", 200)
	u := first["user"].(map[string]any)
	if u["isNew"] != true || u["language"] != "ja" || u["email"] != "example@gmail.com" {
		t.Fatal(first)
	}
	if cfg.DatabaseURL != "" {
		a = open()
	}
	again := post("changed-email", 200)
	actor := again["user"].(map[string]any)
	if actor["id"] != u["id"] || actor["isNew"] != false {
		t.Fatal(again)
	}
	if _, ok, err := a.Accounts.Refresh(t.Context(), first["token"].(string)); ok || err != nil {
		t.Fatal("prior JWT survived")
	}
	if _, ok, err := a.Accounts.Refresh(t.Context(), again["token"].(string)); !ok || err != nil {
		t.Fatal("invalid refresh JWT")
	}
	post("second-subject", 401)
	firstWorkspace := post("workspace", 200)
	post("workspace", 200)
	if users.Load() != 2 || firstWorkspace["user"].(map[string]any)["isNew"] != true {
		t.Fatal("hosted domain or cached subject not checked correctly")
	}
	local, err := a.Accounts.SignUp(t.Context(), "registered@gmail.com", "local-pass", "Local")
	if err != nil {
		t.Fatal(err)
	}
	linked := post("existing", 200)["user"].(map[string]any)
	if linked["isNew"] != false || linked["id"] != strconv.FormatInt(local.User.ID, 10) {
		t.Fatal(linked)
	}
	if _, err := a.Accounts.SignIn(t.Context(), "registered@gmail.com", "local-pass"); err != nil {
		t.Fatal("link changed login password", err)
	}
}
