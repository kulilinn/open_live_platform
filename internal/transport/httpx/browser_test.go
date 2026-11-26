package httpx

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestBrowserOriginAndLimits(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ipchecker", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	for _, tc := range []struct {
		configured, origin string
		allow, creds       string
	}{{"https://app.test", "https://app.test", "https://app.test", "true"}, {"https://app.test", "https://other.test", "", ""}, {"*", "https://app.test", "*", ""}} {
		svc := BrowserHandler(mux, BrowserConfig{Origins: []string{tc.configured}})
		req := httptest.NewRequest(http.MethodOptions, "/ipchecker", nil)
		req.Header.Set("Origin", tc.origin)
		req.Header.Set("Access-Control-Request-Method", "GET")
		w := httptest.NewRecorder()
		svc.ServeHTTP(w, req)
		if w.Code != 204 || w.Header().Get("Access-Control-Allow-Origin") != tc.allow || w.Header().Get("Access-Control-Allow-Credentials") != tc.creds {
			t.Fatalf("preflight %d %v", w.Code, w.Header())
		}
		if tc.allow == "" {
			req = httptest.NewRequest("GET", "/status", nil)
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Upgrade", "websocket")
			w = httptest.NewRecorder()
			svc.ServeHTTP(w, req)
			if w.Code != 403 {
				t.Fatalf("WS origin %d", w.Code)
			}
		}
	}
	trusted := []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	svc := BrowserHandler(mux, BrowserConfig{RateLimit: true, TrustedProxies: trusted})
	req := httptest.NewRequest("GET", "/ipchecker", nil)
	req.RemoteAddr = "192.0.2.12:1000"
	req.Header.Set("X-Forwarded-For", "10.0.0.1, 203.0.113.5, 192.0.2.13")
	if ip := ClientIP(req, trusted); ip.String() != "203.0.113.5" {
		t.Fatal(ip)
	}
	for i := 0; i < 602; i++ {
		w := httptest.NewRecorder()
		svc.ServeHTTP(w, req)
		if (i < 601 && w.Code != 200) || (i == 601 && w.Code != 429) {
			t.Fatalf("request %d = %d", i, w.Code)
		}
	}
	req.Header.Set("X-Forwarded-For", "203.0.113.6")
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatal("another client rate limited")
	}
	now := time.Now()
	var l ipLimiter
	k := ipPath{path: "/status"}
	for i := 0; i < 602; i++ {
		l.allow(k, now)
	}
	if !l.allow(k, now.Add(6*time.Second)) {
		t.Fatal("did not reset after idle window")
	}
}
