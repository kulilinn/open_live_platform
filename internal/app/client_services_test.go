package app_test

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/geo"
	"open_live_platform/internal/httpapi"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func telegramFixture(id int64, token string, at time.Time) string {
	v := url.Values{"auth_date": {strconv.FormatInt(at.Unix(), 10)}, "user": {fmt.Sprintf(`{"id":%d,"first_name":"配信"}`, id)}}
	lines := []string{}
	for k, val := range v {
		lines = append(lines, k+"="+val[0])
	}
	slices.Sort(lines)
	s := hmac.New(sha256.New, []byte("WebAppData"))
	s.Write([]byte(token))
	m := hmac.New(sha256.New, s.Sum(nil))
	m.Write([]byte(strings.Join(lines, "\n")))
	v.Set("hash", hex.EncodeToString(m.Sum(nil)))
	return v.Encode()
}

func TestTelegramMiniApp(t *testing.T) {
	for _, mode := range []string{"memory", "postgres"} {
		for _, family := range []string{"opaque", "jwt"} {
			t.Run(mode+"/"+family, func(t *testing.T) {
				cfg := app.Config{}
				if mode == "postgres" {
					cfg, _ = postgresTestDatabase(t)
				}
				if family == "jwt" {
					cfg.JWTSecret = "test-secret"
				}
				cfg.TelegramBotToken = "fixture:12345"
				open := func() *app.App {
					a, err := app.New(t.Context(), cfg)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(a.Close)
					return a
				}
				a := open()
				native := subscriptionTestUser(t, a, "native", false)
				check := func(id int64, secret string, at time.Time, status int) string {
					t.Helper()
					r := httptest.NewRequest("GET", "/api/auth/tg", nil)
					r.Header.Set("Authorization", "tma "+telegramFixture(id, secret, at))
					w := httptest.NewRecorder()
					a.Server.Handler.ServeHTTP(w, r)
					if w.Code != status {
						t.Fatalf("telegram %d %s; want %d", w.Code, w.Body, status)
					}
					var out struct{ Token string }
					_ = json.Unmarshal(w.Body.Bytes(), &out)
					return out.Token
				}
				const id int64 = 19384732109875
				check(native.User.ID, cfg.TelegramBotToken, time.Now(), 409)
				check(id, "other", time.Now(), 401)
				check(id, cfg.TelegramBotToken, time.Now().Add(-36*time.Hour), 401)
				token := check(id, cfg.TelegramBotToken, time.Now(), 200)
				if mode == "postgres" {
					a = open()
				}
				if family == "jwt" {
					s, ok, err := a.Accounts.Refresh(t.Context(), token)
					if err != nil || !ok {
						t.Fatalf("JWT refresh %v %v", ok, err)
					}
					token = s.Token
				}
				u, ok, err := a.Accounts.Lookup(t.Context(), token)
				if err != nil || !ok || u.ID != id || u.Email != "" || u.Account != "配信" {
					t.Fatalf("user=%+v %v %v", u, ok, err)
				}
				out := subscriptionGet(t, a, auth.Session{Token: token}, "/api/apptoken/app")
				replacement, _ := out["token"].(string)
				current, ok, err := a.Accounts.Refresh(t.Context(), replacement)
				if err != nil || !ok || current.User.ID != id {
					t.Fatalf("app token refresh for %d: %v %v", id, ok, err)
				}
				if family == "jwt" {
					if _, active, err := a.Accounts.Lookup(t.Context(), token); err != nil || active {
						t.Fatal("previous token family still active")
					}
				}
				check(id, cfg.TelegramBotToken, time.Now(), 200)
				other := subscriptionTestUser(t, a, "next", false)
				if other.User.ID <= id {
					t.Fatal("ID sequence not advanced")
				}
			})
		}
	}
}

func TestClientStatusClockPushAndLocation(t *testing.T) {
	var target int64
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]string
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil || r.Method != "POST" || b["user_id"] != strconv.FormatInt(target, 10) || r.Header.Get("Authorization") != "" {
			t.Error("wrong delivery identity")
		}
		switch r.URL.Path {
		case "/key":
			io.WriteString(w, `{"code":200,"data":{"pub_key":"server-key"}}`)
		case "/sub":
			io.WriteString(w, `{"code":200,"data":{}}`)
		default:
			t.Error("incorrect provider path")
		}
	}))
	defer hook.Close()
	cfg := app.Config{CDNKey: "fixture", PreloginEnd: "2100-01-01 00:00:00 +0000", WebPush: httpapi.WebPushConfig{Host: hook.URL, KeyPath: "/key", SubPath: "/sub"}}
	a, err := app.New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	user := subscriptionTestUser(t, a, "browser", false)
	target = user.User.ID
	read := func(serve http.Handler, method, path string, status int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, nil)
		w := httptest.NewRecorder()
		serve.ServeHTTP(w, req)
		if w.Code != status {
			t.Fatalf("%s status %d: %s", path, w.Code, w.Body)
		}
		return w
	}
	mac := fmt.Sprintf("%x", md5.Sum([]byte("123:fixture")))
	for _, method := range []string{"GET", "POST", "OPTIONS"} {
		read(a.Server.Handler, method, "/auth?key=123:"+mac, 200)
	}
	read(a.Server.Handler, "GET", "/auth?key=123:0000", 403)
	if !strings.Contains(read(a.Server.Handler, "GET", "/status", 200).Body.String(), "prelogin") {
		t.Fatal("missing prelogin")
	}
	clock := read(a.Server.Handler, "GET", "/api/time/sync?clientTime=12345", 200)
	if clock.Header().Get("X-Server-Time") == "" || !strings.Contains(clock.Body.String(), `"clientTime":"12345"`) {
		t.Fatal(clock.Body.String())
	}
	r := authFlowRequest(t, a.Server.Handler, "/api/notification/webpush/key", user.Token, nil, 200)
	if r["pub_key"] != "server-key" {
		t.Fatal(r)
	}
	r = authFlowRequest(t, a.Server.Handler, "/api/notification/webpush/subscription", user.Token, map[string]any{"user_id": "99999", "endpoint": "https://push.example/sub", "p256dh": "pub", "auth": "auth"}, 200)
	if r["status"] != "200" {
		t.Fatal(r)
	}
	mux := http.NewServeMux()
	httpapi.RegisterGeo(mux, geo.Config{Filter: "JP;CN", XPT: "entry", TrustedProxies: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}}, func(ip netip.Addr) string {
		if ip.String() == "203.0.113.9" {
			return "JP"
		}
		return "US"
	})
	for _, tc := range []struct{ proxy, status string }{{"192.0.2.1:80", "403"}, {"198.51.100.1:80", "200"}} {
		req := httptest.NewRequest("GET", "/ipchecker_v1", nil)
		req.RemoteAddr = tc.proxy
		req.Header.Set("X-Forwarded-For", "203.0.113.9")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if !strings.Contains(w.Body.String(), tc.status) {
			t.Fatal(w.Body.String())
		}
	}
}
