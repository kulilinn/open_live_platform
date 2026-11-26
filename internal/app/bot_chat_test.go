package app_test

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/botchat"
	"open_live_platform/internal/relay"
)

func TestBotChatParticipantsAndCursor(t *testing.T) {
	for _, mode := range []string{"memory", "postgres"} {
		t.Run(mode, func(t *testing.T) {
			cfg := app.Config{}
			if mode == "postgres" {
				cfg, _ = postgresTestDatabase(t)
			}
			cfg.RelaySecret = "fixture-bot-chat"
			cfg.RelayRust = relay.RustOptions{AppKey: "bot-chat", AppSecret: "fixture"}
			a, err := app.New(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(a.Close)
			other := a
			if mode == "postgres" {
				other, err = app.New(t.Context(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(other.Close)
			}
			host := httptest.NewServer(other.Server.Handler)
			t.Cleanup(host.Close)
			user := subscriptionTestUser(t, a, "author", false)
			bot := subscriptionTestUser(t, a, "bot", false)
			third := subscriptionTestUser(t, a, "third", false)
			path := "/api/chatbot/" + strconv.FormatInt(bot.User.ID, 10)
			socketA := openPrivateH5(t, host, user)
			defer socketA.Close()
			socketB := openPrivateH5(t, host, bot)
			defer socketB.Close()
			post := func(body string) string {
				r := httptest.NewRequest("POST", path, strings.NewReader(body))
				r.Header.Set("Authorization", "Bearer "+user.Token)
				w := httptest.NewRecorder()
				a.Server.Handler.ServeHTTP(w, r)
				var out struct{ Status, Data string }
				if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 || out.Status != "200" {
					t.Fatalf("%d %s %v", w.Code, w.Body, err)
				}
				return out.Data
			}
			id := post(`{"request":"hello"}`)
			for _, s := range []*websocket.Conn{socketA, socketB} {
				var frame struct {
					Content, Queue string
					Payload        botchat.Message
				}
				for frame.Content != "botchat" {
					if err := s.ReadJSON(&frame); err != nil {
						t.Fatal(err)
					}
				}
				if frame.Queue != strconv.FormatInt(user.User.ID, 10) || frame.Payload.ID != id || frame.Payload.From != user.User.ID || frame.Payload.Language != "ja" {
					t.Fatalf("frame %+v", frame)
				}
			}
			for i := 0; i < 20; i++ {
				post(fmt.Sprintf(`{"request":"%d","language":"zh"}`, i))
			}
			history := func(account auth.Session, url string, status int) botchat.Page {
				r := httptest.NewRequest("GET", url, nil)
				r.Header.Set("Authorization", "Bearer "+account.Token)
				w := httptest.NewRecorder()
				other.Server.Handler.ServeHTTP(w, r)
				if w.Code != status {
					t.Fatalf("history %d %s; want %d", w.Code, w.Body, status)
				}
				var p botchat.Page
				if status == 200 {
					if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || p.Data == nil {
						t.Fatalf("invalid data %s %v", w.Body, err)
					}
				}
				return p
			}
			p := history(user, path+"/latest", 200)
			if len(p.Data) != 20 || p.More == "" {
				t.Fatalf("first page %+v", p)
			}
			next := history(user, path+"/"+p.More, 200)
			if len(next.Data) != 1 || next.More != "" || next.Data[0].ID <= p.Data[19].ID {
				t.Fatalf("next %+v", next)
			}
			if got := history(third, path+"/latest", 200); len(got.Data) != 0 {
				t.Fatal("leaked another conversation")
			}
			history(third, path+"/"+p.More, 400)
			p = history(bot, "/api/chatbot/"+strconv.FormatInt(user.User.ID, 10)+"/latest", 200)
			if len(p.Data) != 20 {
				t.Fatal("recipient history missing")
			}
		})
	}
}
