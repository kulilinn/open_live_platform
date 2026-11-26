package app_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/date"
	"open_live_platform/internal/gift"
	"open_live_platform/internal/relay"
)

func TestPostgresGiftInteractions(t *testing.T) {
	config, db := postgresTestDatabase(t)
	ctx := t.Context()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var charges atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/payment/user/point":
			fmt.Fprint(w, `{"code":200,"data":{"point":1500}}`)
		case "POST /api/payment/point/decrease":
			var body struct {
				Point   int64
				OrderNo string `json:"order_no"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || !strings.HasPrefix(body.OrderNo, "date-gift-") {
				http.Error(w, "unexpected charge", 400)
				return
			}
			id := charges.Add(1)
			fmt.Fprintf(w, `{"code":200,"data":{"order_id":"%d","order_no":"provider-%d","total_point":%d}}`, id, id, body.Point)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(provider.Close)
	config.PaymentHost, config.PaymentAppID = provider.URL, "interactions"
	config.RelayRust = relay.RustOptions{AppKey: "gift-interactions", AppSecret: "gift-interactions-test"}
	config.GiftCatalogFile = filepath.Join(t.TempDir(), "gifts.json")
	check(os.WriteFile(config.GiftCatalogFile, []byte(`[
		{"id":"801","price":20,"name":"Rock Paper Scissors","giftType":1,"effectControlType":1},
		{"id":"802","price":10,"name":"Blocked Flower"},
		{"id":"803","price":35,"name":"Guess","giftType":1,"effectControlType":1},
		{"id":"804","price":10,"name":"Recommendation"}]`), 0600))
	config.GiftInteractionFile = filepath.Join(t.TempDir(), "interactions.json")
	now := time.Now().UTC()
	from, to := now.Add(-time.Hour), now.Add(time.Hour)
	used := int64(1)
	options := gift.Interactions{
		Promotions: []gift.Promotion{{ID: 41, GiftID: 804, Name: "Spring", Starts: from, Ends: to}},
		Limits: []gift.Limit{
			{ID: 701, GiftID: 804, PromotionID: 41, Price: 5, Discount: 50, Listing: 1, Starts: from, Ends: to},
			{ID: 702, GiftID: 804, PromotionID: 41, Price: 5, Listing: 1, Starts: from, Ends: to, Platform: 1, PlatformUsage: &used},
			{ID: 703, GiftID: 804, PromotionID: 41, Price: 5, Listing: 1, Starts: from, Ends: to, PerUser: 1},
		},
		Boxes: []gift.Box{{ID: 501, LimitIDs: "701,702,703"}},
	}
	choice1, choice2 := "one", "two"
	for i := 1; i <= 5; i++ {
		options.Questions = append(options.Questions, gift.Question{ID: int64(i), Subject: fmt.Sprintf("Question %d", i),
			Target: 1 + int8(i%2), AnswerOne: &choice1, AnswerTwo: &choice2})
	}
	body, err := json.Marshal(options)
	check(err)
	check(os.WriteFile(config.GiftInteractionFile, body, 0600))
	open := func() *app.App {
		t.Helper()
		a, err := app.New(ctx, config)
		check(err)
		t.Cleanup(a.Close)
		return a
	}
	first, second := open(), open()
	name := func(who string) auth.Session {
		t.Helper()
		s, err := first.Accounts.SignUp(ctx, who+"@gift-game.test", "test-pass", who)
		check(err)
		return s
	}
	host, fan, outside := name("host"), name("fan"), name("outside")
	session := date.Session{Channel: date.NewChannel(), AnchorID: host.User.ID, AudienceID: fan.User.ID,
		StartedAt: now, AnchorSeen: now.Add(time.Minute), AudienceSeen: now.Add(time.Minute)}
	_, err = db.Exec(ctx, `INSERT INTO date_sessions(channel,anchor_id,audience_id,state) VALUES($1,$2,$3,$4)`,
		session.Channel, session.AnchorID, session.AudienceID, session)
	check(err)
	channel := strconv.FormatInt(session.Channel, 10)
	httpServer := httptest.NewServer(second.Server.Handler)
	t.Cleanup(httpServer.Close)
	socket := openPrivateH5(t, httpServer, fan)
	t.Cleanup(func() { socket.Close() })
	next := func(content string) json.RawMessage {
		t.Helper()
		check(socket.SetReadDeadline(time.Now().Add(5 * time.Second)))
		for {
			var frame struct {
				Content string
				Payload json.RawMessage
			}
			check(socket.ReadJSON(&frame))
			if frame.Content == content {
				return frame.Payload
			}
		}
	}
	request := func(a *app.App, who auth.Session, method, path, input string, code int, status string) json.RawMessage {
		t.Helper()
		r := httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(input))
		r.Header.Set("Authorization", "Bearer "+who.Token)
		w := httptest.NewRecorder()
		a.Server.Handler.ServeHTTP(w, r)
		var result struct {
			Status string
			Data   json.RawMessage
		}
		check(json.Unmarshal(w.Body.Bytes(), &result))
		if w.Code != code || result.Status != status {
			t.Fatalf("%s = %d %s, want %d %q", path, w.Code, w.Body.String(), code, status)
		}
		return result.Data
	}
	const api = "/api/gift/"
	block := fmt.Sprintf(`{"dateRoomId":%q,"giftIds":["802","802"]}`, channel)
	request(first, fan, "POST", api+"block", block, 403, "403")
	request(first, host, "POST", api+"block", block, 200, "200")
	request(second, host, "POST", api+"block", block, 200, "200")
	if data := request(second, fan, "GET", "/api/date/gifts/"+channel, "", 200, "200"); strings.Contains(string(data), "Blocked Flower") {
		t.Fatalf("blocked gift leaked in date catalog: %s", data)
	}
	send := func(item int, code int, status string) {
		t.Helper()
		body := fmt.Sprintf(`{"dateRoomId":%q,"giftId":"%d","senderId":"%d","receiverId":"%d","quantity":1}`,
			channel, item, fan.User.ID, host.User.ID)
		request(first, fan, "POST", "/api/date/give/gifts", body, code, status)
	}
	send(802, 403, "403")
	recommend := api + "recommend/" + channel
	request(first, fan, "POST", recommend, `{"giftId":"804","giftType":0}`, 403, "403")
	request(first, host, "POST", recommend, `{"giftId":"802","giftType":0}`, 403, "403")
	request(first, host, "POST", recommend, `{"giftId":"804","giftType":0}`, 200, "200")
	if data := next("recommend_gifts"); !strings.Contains(string(data), "Recommendation") {
		t.Fatalf("recommendation frame = %s", data)
	}
	data := request(first, fan, "GET", api+"promotions/info?date_room_id="+channel, "", 200, "200")
	if !strings.Contains(string(data), `"promotions_name":"Spring"`) || !strings.Contains(string(data), `"discount_price":5`) {
		t.Fatalf("promotion catalog = %s", data)
	}
	box := api + "box/info?box_id=501&user_id=" + strconv.FormatInt(fan.User.ID, 10)
	request(first, host, "GET", box, "", 403, "403")
	data = request(first, fan, "GET", box, "", 200, "200")
	if !strings.Contains(string(data), `"limit_id":"701"`) || strings.Contains(string(data), `"limit_id":"702"`) ||
		strings.Contains(string(data), `"limit_id":"703"`) {
		t.Fatalf("gift box quantities = %s", data)
	}
	data = request(first, fan, "GET", api+"game/list", "", 200, "200")
	if strings.Count(string(data), "Question ") != 5 {
		t.Fatalf("gift questions = %s", data)
	}
	verify := fmt.Sprintf(`{"dateRoomId":%q,"giftId":"801","userId":"%d"}`, channel, fan.User.ID)
	request(first, outside, "POST", api+"game/verif", verify, 403, "403")
	request(first, fan, "POST", api+"game/verif", verify, 200, "200")
	vote := func(a *app.App, who auth.Session, item, kind, role int, accept bool, code int, status string) {
		t.Helper()
		body := fmt.Sprintf(`{"dateRoomId":%q,"giftId":"%d","userId":"%d","anchorId":"%d","gameType":%d,"playerType":%d,"acceptFlag":%t}`,
			channel, item, fan.User.ID, host.User.ID, kind, role, accept)
		request(a, who, "POST", api+"game/accept", body, code, status)
	}
	vote(first, outside, 801, 0, 2, true, 403, "403")
	vote(first, fan, 801, 0, 1, true, 403, "403")
	vote(first, fan, 801, 0, 2, true, 200, "200")
	if charges.Load() != 0 {
		t.Fatal("charged after one participant voted")
	}
	vote(second, host, 801, 0, 1, true, 200, "200")
	vote(first, host, 801, 0, 1, true, 200, "200")
	if charges.Load() != 1 {
		t.Fatalf("game should charge once, got %d", charges.Load())
	}
	data = next("gift_game")
	if !strings.Contains(string(data), `"status":"200"`) {
		t.Fatalf("game start = %s", data)
	}
	move := func(a *app.App, who auth.Session, kind string, role int, payload string, status string, code int) {
		t.Helper()
		body := fmt.Sprintf(`{"dateRoomId":%q,"playerId":"%d","playerType":%d,"gameType":%s}`,
			channel, who.User.ID, role, payload)
		request(a, who, "POST", api+"game/"+kind, body, code, status)
	}
	move(first, host, "quan", 1, `{"quan":1}`, "200", 200)
	move(second, fan, "quan", 2, `{"quan":2}`, "200", 200)
	data = next("gift_game")
	if !strings.Contains(string(data), `"winner":1`) || !strings.Contains(string(data), `"winnerScore":1`) {
		t.Fatalf("RPS result = %s", data)
	}

	send(803, 200, "200")
	vote(first, host, 803, 1, 1, true, 200, "200")
	vote(second, fan, 803, 1, 2, true, 200, "200")
	data = next("gift_game")
	if !strings.Contains(string(data), "Question") {
		t.Fatalf("guess game questions = %s", data)
	}
	for i := 1; i <= 5; i++ {
		body := fmt.Sprintf(`{"guess":{"questionId":"%d","answer":"one","score":500}}`, i)
		move(first, host, "guess", 1, body, "200", 200)
		move(second, fan, "guess", 2, body, "200", 200)
	}
	data = next("gift_game")
	if !strings.Contains(string(data), `"winnerScore":3`) || !strings.Contains(string(data), `"loserScore":2`) ||
		!strings.Contains(string(data), `"winner":1`) {
		t.Fatalf("guess score used client claims: %s", data)
	}
	send(801, 200, "200")
	vote(first, fan, 801, 0, 2, true, 200, "200")
	vote(second, host, 801, 0, 1, false, 200, "200")
	data = next("gift_game")
	if !strings.Contains(string(data), `"reject_person":1`) || charges.Load() != 2 {
		t.Fatalf("declined game frame = %s, charge count %d", data, charges.Load())
	}
	request(first, fan, "POST", api+"game/verif", verify, 200, "200")
	vote(first, fan, 801, 0, 2, true, 200, "200")
	vote(second, host, 801, 0, 1, true, 200, "200")
	if charges.Load() != 3 {
		t.Fatalf("new verified round did not get its own payment: %d", charges.Load())
	}
}
