package httpapi

import (
	"bytes"
	"crypto/md5"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/transport/httpx"
	"strconv"
	"strings"
	"time"
)

type WebPushConfig struct{ Host, KeyPath, SubPath string }

func RegisterClientServices(mux *http.ServeMux, accounts auth.AccountStore, cdnKey string, push WebPushConfig) {
	s := server{accounts: accounts}
	for _, method := range []string{"GET", "POST", "OPTIONS"} {
		mux.HandleFunc(method+" /auth", func(w http.ResponseWriter, r *http.Request) {
			ts, mac, ok := strings.Cut(r.URL.Query().Get("key"), ":")
			sig, err := hex.DecodeString(mac)
			expected := md5.Sum([]byte(ts + ":" + cdnKey))
			if !ok || ts == "" || cdnKey == "" || err != nil || subtle.ConstantTimeCompare(sig, expected[:]) != 1 {
				w.WriteHeader(403)
				return
			}
			w.WriteHeader(200)
		})
	}
	mux.HandleFunc("GET /api/time/sync", func(w http.ResponseWriter, r *http.Request) {
		received := time.Now().UnixMilli()
		client, err := strconv.ParseInt(r.URL.Query().Get("clientTime"), 10, 64)
		if err != nil {
			httpx.Error(w, 400, "clientTime required")
			return
		}
		now := time.Now().UnixMilli()
		n := func(v int64) string { return strconv.FormatInt(v, 10) }
		delay := now - received
		w.Header().Set("X-Server-Time", n(now))
		w.Header().Set("X-Request-Received-Time", n(received))
		w.Header().Set("X-Processing-Delay", n(delay))
		w.Header().Set("Cache-Control", "no-store")
		httpx.JSON(w, 200, map[string]any{"status": "200", "data": map[string]any{"requestReceivedTime": n(received), "responseTime": n(now), "processingDelay": n(delay), "serverTimeSeconds": n(now / 1000), "timezoneOffset": 0, "clientTime": n(client), "estimatedRtt": n(now - client + delay)}})
	})
	client := http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, action := range []string{"key", "subscription"} {
		mux.HandleFunc("POST /api/notification/webpush/"+action, s.authenticated(func(w http.ResponseWriter, r *http.Request, uid int64) {
			in := map[string]string{"user_id": strconv.FormatInt(uid, 10)}
			code, path := "4051", push.KeyPath
			if action == "subscription" {
				var sub struct{ Endpoint, Auth, P256DH string }
				if !httpx.Decode(w, r, &sub) {
					return
				}
				if sub.Endpoint == "" || sub.Auth == "" || sub.P256DH == "" {
					httpx.Error(w, 400, "incomplete subscription")
					return
				}
				in["endpoint"], in["auth"], in["p256dh"] = sub.Endpoint, sub.Auth, sub.P256DH
				code, path = "4052", push.SubPath
			}
			if push.Host == "" || path == "" {
				httpx.Error(w, 503, "web push provider unavailable")
				return
			}
			endpoint, err := url.JoinPath(push.Host, path)
			fail := func() {
				httpx.JSON(w, 200, map[string]string{"status": code, "message": "web push provider request failed"})
			}
			if err != nil {
				fail()
				return
			}
			b, _ := json.Marshal(in)
			req, err := http.NewRequestWithContext(r.Context(), "POST", endpoint, bytes.NewReader(b))
			if err != nil {
				fail()
				return
			}
			req.Header.Set("Content-Type", "application/json")
			res, err := client.Do(req)
			if err != nil {
				fail()
				return
			}
			defer res.Body.Close()
			var out struct {
				Code int
				Data map[string]string
			}
			if res.StatusCode/100 != 2 || json.NewDecoder(io.LimitReader(res.Body, 256<<10)).Decode(&out) != nil || out.Code != 200 {
				fail()
				return
			}
			response := map[string]string{"status": "200"}
			if action == "key" {
				if out.Data["pub_key"] == "" {
					fail()
					return
				}
				response["pub_key"] = out.Data["pub_key"]
			}
			httpx.JSON(w, 200, response)
		}))
	}
}
