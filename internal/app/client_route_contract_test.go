package app_test

import (
	_ "embed"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"open_live_platform/internal/app"
	"open_live_platform/internal/relay"
)

//go:embed testdata/livemedia_client_routes.txt
var clientRouteContract string

//go:embed testdata/livemedia_service_routes.txt
var serviceRouteContract string

//go:embed testdata/livemedia_admin_routes.txt
var adminRouteContract string

//go:embed testdata/livemedia_admin_mcn_routes.txt
var mcnRouteContract string

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
	assertRustRoutes(t, a.Server.Handler, clientRouteContract)
	assertRustRoutes(t, a.Server.Handler, serviceRouteContract)
}

func TestRustBackofficeRoutesRegistered(t *testing.T) {
	a := startEditorialAPI(t, func(cfg *app.Config) {
		cfg.MCNAdminAddr = "127.0.0.1:0"
		cfg.MCNInternalToken = "route-test-token"
		cfg.PaymentHost, cfg.PaymentAppID = "http://127.0.0.1:1", "route-test"
	})
	t.Run("admin", func(t *testing.T) { assertRustRoutes(t, a.operator, adminRouteContract) })
	t.Run("mcn", func(t *testing.T) { assertRustRoutes(t, a.platform.MCNBackoffice.Handler, mcnRouteContract) })
}

func assertRustRoutes(t *testing.T, handler http.Handler, manifest string) {
	t.Helper()
	mux := handler.(interface {
		Handler(*http.Request) (http.Handler, string)
	})
	param := regexp.MustCompile(`\{[^}]+\}`)
	normalize := func(s string) string { return param.ReplaceAllString(s, "{}") }
	for _, line := range strings.Split(strings.TrimSpace(manifest), "\n") {
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
