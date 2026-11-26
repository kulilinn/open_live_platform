package app_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"open_live_platform/internal/app"
	"open_live_platform/internal/notice"
)

func TestNoticeHTTP(t *testing.T) {
	for _, mode := range []string{"memory", "postgres"} {
		t.Run(mode, func(t *testing.T) {
			cfg := app.Config{}
			if mode == "postgres" {
				cfg, _ = postgresTestDatabase(t)
			}
			cfg.EdgeToken = "notice-producer"
			a, err := app.New(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			receiver, err := a.Accounts.SignUp(t.Context(), "receiver@example.org", "password", "Receiver")
			if err != nil {
				t.Fatal(err)
			}
			other, err := a.Accounts.SignUp(t.Context(), "other@example.org", "password", "Other")
			if err != nil {
				t.Fatal(err)
			}
			request := func(method, path, token, body string, want int) []byte {
				t.Helper()
				r := httptest.NewRequest(method, path, strings.NewReader(body))
				r.Header.Set("Authorization", "Bearer "+token)
				w := httptest.NewRecorder()
				a.Server.Handler.ServeHTTP(w, r)
				if w.Code != want {
					t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body)
				}
				return w.Body.Bytes()
			}
			const id = int64(912345678901234567)
			payload := fmt.Sprintf(`{"type":"notice","notice":{"id":"%d","userId":"%d","noticeTarget":2,"noticeType":1,"objectId":"%d","content":{"title":"開播"},"createTime":"2026-01-01T00:00:00Z"}}`, id, receiver.User.ID, id+1)
			send := func() { request("POST", "/api/edge/events", cfg.EdgeToken, payload, 204) }
			count := func(token string, want int64) {
				t.Helper()
				raw := request("GET", "/api/notice/get_unread_notice_count", token, "", 200)
				var res struct {
					Status string
					Data   int64
				}
				if err := json.Unmarshal(raw, &res); err != nil || res.Status != "200" || res.Data != want {
					t.Fatalf("unread %s, %v", raw, err)
				}
			}
			request("POST", "/api/edge/events", receiver.Token, payload, http.StatusUnauthorized)
			send()
			send()
			count(receiver.Token, 1)
			count(other.Token, 0)
			request("GET", "/api/notice/read_notice", other.Token, "", 200)
			count(receiver.Token, 1)
			request("GET", "/api/notice/read_notice", receiver.Token, "", 200)
			send()
			count(receiver.Token, 0)
			raw := request("GET", "/api/notice/page_notice?page=0&pageSize=200", receiver.Token, "", 200)
			var res struct{ Data notice.Page }
			if err := json.Unmarshal(raw, &res); err != nil {
				t.Fatal(err)
			}
			p := res.Data
			if p.Total != 1 || p.Current != 1 || p.Size != 100 || len(p.List) != 1 || p.List[0].Status != 2 || p.List[0].ID != id || p.List[0].ObjectID == nil || *p.List[0].ObjectID != id+1 {
				t.Fatalf("page %s", raw)
			}
			empty := request("GET", "/api/notice/page_notice?page=99&pageSize=1", receiver.Token, "", 200)
			if !strings.Contains(string(empty), `"list":[]`) {
				t.Fatalf("empty page %s", empty)
			}
		})
	}
}
