package app_test

import (
	"net/http"
	"testing"

	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
)

func TestLoginCodeExchange(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		testLoginCodeExchange(t, app.Config{}, nil)
	})
	t.Run("postgres", func(t *testing.T) {
		config, db := postgresTestDatabase(t)
		testLoginCodeExchange(t, config, func() {
			if _, err := db.Exec(t.Context(), "UPDATE auth_login_grants SET expires_at=now()"); err != nil {
				t.Fatal(err)
			}
		})
	})
}

func testLoginCodeExchange(t *testing.T, config app.Config, expire func()) {
	t.Helper()
	config.JWTSecret = "test-jwt-exchange"
	start := func() *app.App {
		t.Helper()
		platform, err := app.New(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(platform.Close)
		return platform
	}
	issuer := start()
	consumer := issuer
	if config.DatabaseURL != "" {
		consumer = start()
	}
	const email, password = "exchange@example.org", "current-password"
	opaque, err := issuer.Accounts.SignUp(t.Context(), email, password, "Exchange")
	if err != nil {
		t.Fatal(err)
	}
	refresh, err := issuer.Accounts.(*auth.JWTAccounts).SignInJWT(t.Context(), email, password)
	if err != nil {
		t.Fatal(err)
	}
	access := func(refresh string) string {
		t.Helper()
		session, ok, err := issuer.Accounts.Refresh(t.Context(), refresh)
		if err != nil || !ok || session.User.ID != opaque.User.ID {
			t.Fatalf("refresh = %t, user ID %d, error %v", ok, session.User.ID, err)
		}
		return session.Token
	}
	issue := func(access string, status int) map[string]any {
		t.Helper()
		return authFlowRequest(t, issuer.Server.Handler, "/api/auth/exchange_code", access, nil, status)
	}
	consume := func(code string, status int) map[string]any {
		t.Helper()
		return authFlowRequest(t, consumer.Server.Handler, "/api/auth/exchange_token", "", map[string]any{"code": code, "email": "unrelated@example.org"}, status)
	}
	issue("", http.StatusUnauthorized)
	issue(opaque.Token, http.StatusUnauthorized)
	issue(refresh.Token, http.StatusUnauthorized)
	consume("invented", http.StatusUnauthorized)
	previousAccess := access(refresh.Token)
	code := issue(previousAccess, http.StatusOK)["data"].(map[string]any)["code"].(string)
	if len(code) != 32 {
		t.Fatalf("exchange code len = %d, want 32", len(code))
	}
	exchanged := consume(code, http.StatusOK)["token"].(string)
	consume(code, http.StatusUnauthorized)
	if _, ok, err := issuer.Accounts.Lookup(t.Context(), previousAccess); err != nil || ok {
		t.Fatalf("old access survived exchange: accepted = %t, error = %v", ok, err)
	}
	newAccess := access(exchanged)
	if expire != nil {
		expired := issue(newAccess, http.StatusOK)["data"].(map[string]any)["code"].(string)
		expire()
		consume(expired, http.StatusUnauthorized)
	}
	stale := issue(newAccess, http.StatusOK)["data"].(map[string]any)["code"].(string)
	authFlowRequest(t, issuer.Server.Handler, "/api/auth/signout", newAccess, nil, http.StatusOK)
	consume(stale, http.StatusUnauthorized)
}
