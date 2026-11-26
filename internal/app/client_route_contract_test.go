package app_test

import (
	_ "embed"
	"net/http"
	"net/http/httptest"
	"open_live_platform/internal/app"
	"open_live_platform/internal/relay"
	"regexp"
	"strings"
	"testing"
)

//go:embed testdata/livemedia_client_routes.txt
var clientRouteContract string

// This only verifies dispatch. Business and response compatibility are exercised in the feature tests.
func TestRustClientRoutesRegistered(t *testing.T) {
	cfg, _ := postgresTestDatabase(t)
	cfg.RelayRust = relay.RustOptions{AppID: 777, AppKey: "local-application", AppSecret: "fixture", AppUsers: []int64{777}}
	cfg.PaymentHost, cfg.PaymentAppID, cfg.PaymentProxyRoutes = "http://127.0.0.1:1", "test-app", "GET /api/payment/777,POST /api/payment/777"
	a, err := app.New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	mux := a.Server.Handler.(*http.ServeMux)
	param := regexp.MustCompile(`\{[^}]+\}`)
	normalize := func(s string) string { return param.ReplaceAllString(s, "{}") }
	for _, line := range strings.Split(strings.TrimSpace(clientRouteContract), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		method, path, _ := strings.Cut(line, " ")
		probe := param.ReplaceAllString(path, "777")
		_, pattern := mux.Handler(httptest.NewRequest(method, probe, nil))
		if normalize(pattern) != normalize(line) {
			t.Errorf("%s dispatches to %q", line, pattern)
		}
	}
}
