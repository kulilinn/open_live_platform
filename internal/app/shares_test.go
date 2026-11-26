package app_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/feed"
	"open_live_platform/internal/upload"
)

func TestPublicShares(t *testing.T) {
	for _, mode := range []string{"memory", "postgres"} {
		t.Run(mode, func(t *testing.T) {
			var writes atomic.Int64
			mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, err := io.ReadAll(r.Body)
				if err != nil || len(b) == 0 || !strings.HasPrefix(r.URL.Path, "/media/assets/shares/") || !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-") {
					t.Error("invalid object put")
				}
				writes.Add(1)
			}))
			defer mock.Close()
			cfg := app.Config{}
			if mode == "postgres" {
				cfg, _ = postgresTestDatabase(t)
			}
			cfg.Upload = upload.Config{Bucket: "media", BaseURL: "https://cdn.example", Endpoint: mock.URL, KeyID: "test", Secret: "fixture"}
			open := func() *app.App {
				a, err := app.New(t.Context(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(a.Close)
				return a
			}
			a := open()
			u := subscriptionTestUser(t, a, "owner", false)
			other := subscriptionTestUser(t, a, "stranger", false)
			write := func(who auth.Session, path, id, content string, want int) feed.Share {
				t.Helper()
				var buf bytes.Buffer
				form := multipart.NewWriter(&buf)
				if id != "" {
					form.WriteField("id", id)
				}
				form.WriteField("content", content)
				f, _ := form.CreateFormFile("photo", "example.jpg")
				fmt.Fprint(f, "photo")
				form.Close()
				req := httptest.NewRequest("POST", "/api/shares/"+path, &buf)
				req.Header.Set("Content-Type", form.FormDataContentType())
				req.Header.Set("Authorization", "Bearer "+who.Token)
				w := httptest.NewRecorder()
				a.Server.Handler.ServeHTTP(w, req)
				if w.Code != want {
					t.Fatalf("%s = %d %s", path, w.Code, w.Body)
				}
				var res struct{ Data feed.Share }
				if path == "add" {
					if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
						t.Fatal(err)
					}
				}
				return res.Data
			}
			share := write(u, "add", "", "投稿", 200)
			if share.ID <= 0 || share.UserID != u.User.ID || share.Status != 1 || share.Content != "投稿" || strings.Contains(share.Photo, "/") {
				t.Fatal(share)
			}
			id := strconv.FormatInt(share.ID, 10)
			if mode == "postgres" {
				a = open()
			}
			write(other, "update", id, "forbidden", 403)
			authFlowRequest(t, a.Server.Handler, "/api/shares/delete", other.Token, map[string]any{"id": id}, 403)
			write(u, "update", id, "更新", 200)
			list := func(view, owner string) map[string]any {
				t.Helper()
				return authFlowRequest(t, a.Server.Handler, "/api/shares/list", "", map[string]any{"offset": 0, "share": view, "uid": owner}, 200)
			}
			got := list("hot", "")
			if got["cursor"] != float64(1) {
				t.Fatal(got)
			}
			item := got["data"].([]any)[0].(map[string]any)
			if item["content"] != "更新" || item["status"] != float64(2) || !strings.HasPrefix(item["photo"].(string), "https://cdn.example/assets/shares/") {
				t.Fatal(item)
			}
			if len(list("my", strconv.FormatInt(other.User.ID, 10))["data"].([]any)) != 0 {
				t.Fatal("unexpected owner match")
			}
			authFlowRequest(t, a.Server.Handler, "/api/shares/delete", u.Token, map[string]any{"id": id}, 200)
			if len(list("new", "")["data"].([]any)) != 0 || writes.Load() != 2 {
				t.Fatal("unexpected visible row or writes")
			}
			w := httptest.NewRecorder()
			a.Server.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/share?title=Demo&redirect=%2Froom", nil))
			if w.Code != 200 || !strings.Contains(w.Body.String(), `property="og:title" content="Demo"`) || !strings.Contains(w.Body.String(), `location.href="/room"`) {
				t.Fatal(w.Body.String())
			}
		})
	}
}
