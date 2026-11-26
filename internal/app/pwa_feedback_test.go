package app_test

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"open_live_platform/internal/app"
	"open_live_platform/internal/catalog"
	"open_live_platform/internal/store/postgres"
)

func TestPWASubscriptionsAndFeedback(t *testing.T) {
	for _, mode := range []string{"memory", "postgres"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				if r.Method != "POST" || !strings.Contains(string(b), "配信") || !strings.Contains(string(b), "msg_type") {
					t.Error("unexpected notification")
				}
				calls.Add(1)
				io.WriteString(w, `{"code":0}`)
			}))
			defer hook.Close()
			cfg := app.Config{}
			if mode == "postgres" {
				cfg, _ = postgresTestDatabase(t)
			}
			cfg.FeedbackHook = hook.URL
			data := catalog.Snapshot{}
			cfg.PublicCatalog = data
			open := func() *app.App {
				a, err := app.New(t.Context(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(a.Close)
				return a
			}
			a := open()
			anchor := subscriptionTestUser(t, a, "host", true)
			fan := subscriptionTestUser(t, a, "viewer", false)
			id := strconv.FormatInt(anchor.User.ID, 10)
			err := json.Unmarshal([]byte(`{"pwa":[{"id":`+id+`,"pwa_id":"demo","icon":"test.png","game_url":"https://example.org/game","weight":1,"works":"x","image":"y"}]}`), &data)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "postgres" {
				db, err := postgres.Open(t.Context(), cfg.DatabaseURL)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Shutdown()
				if err = db.ReplacePublicCatalog(t.Context(), data); err != nil {
					t.Fatal(err)
				}
			}
			if res := subscriptionGet(t, a, fan, "/api/get_anchor?search_name=HOST"); res["data"] != id {
				t.Fatal(res)
			}
			if res := subscriptionGet(t, a, fan, "/api/get_anchor?search_name=viewer"); res["status"] != "4005" {
				t.Fatal(res)
			}
			entry := func() map[string]any {
				return subscriptionGet(t, a, fan, "/api/pwa/anchor")["data"].([]any)[0].(map[string]any)
			}
			if e := entry(); e["id"] != id || e["sub_status"] != false {
				t.Fatal(e)
			}
			authFlowRequest(t, a.Server.Handler, "/api/sub/anchor/create_free", anchor.Token, map[string]any{}, 200)
			for range 2 {
				got := authFlowRequest(t, a.Server.Handler, "/api/pwa/sub", fan.Token, map[string]any{"id": []string{id}}, 200)
				if got["status"] != "200" {
					t.Fatal(got)
				}
			}
			if e := entry(); e["sub_status"] != true {
				t.Fatal(e)
			}
			submit := func(content string, want int) {
				t.Helper()
				var b bytes.Buffer
				f := multipart.NewWriter(&b)
				f.WriteField("content", content)
				f.WriteField("feedback_type", "3")
				f.Close()
				r := httptest.NewRequest("POST", "/api/feedback/submit", &b)
				r.Header.Set("Authorization", "Bearer "+fan.Token)
				r.Header.Set("Content-Type", f.FormDataContentType())
				w := httptest.NewRecorder()
				a.Server.Handler.ServeHTTP(w, r)
				if w.Code != want {
					t.Fatalf("feedback %d %s", w.Code, w.Body)
				}
			}
			submit(strings.Repeat("界", 1001), 500)
			submit("配信が止まりました", 200)
			if mode == "postgres" {
				a = open()
			}
			submit("配信が止まりました", 500)
			if calls.Load() != 1 {
				t.Fatalf("hook called %d times", calls.Load())
			}
		})
	}
}
