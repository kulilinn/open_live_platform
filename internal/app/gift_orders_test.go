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

	"github.com/gorilla/websocket"
	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/gift"
)

func TestGiftOrders(t *testing.T) {
	testGiftOrders(t, app.Config{}, false)
}

func TestPostgresGiftOrders(t *testing.T) {
	config, _ := postgresTestDatabase(t)
	testGiftOrders(t, config, true)
}

func testGiftOrders(t *testing.T, config app.Config, acrossInstances bool) {
	t.Helper()
	ctx := t.Context()
	const giftID = "9007199254740995"
	const price int64 = 4294967297
	const startBalance int64 = 9007199254740993
	var balance atomic.Int64
	balance.Store(startBalance)
	var charges atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		uid, _ := strconv.ParseInt(r.Header.Get("USER_ID"), 10, 64)
		if r.Header.Get("APPID") != "test-payment-app" || uid <= 0 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/payment/user/point":
			fmt.Fprintf(w, `{"code":200,"data":{"point":%d}}`, balance.Load())
		case "POST /api/payment/point/decrease":
			var debit struct {
				UserID  int64  `json:"user_id"`
				Point   int64  `json:"point"`
				OrderNo string `json:"order_no"`
			}
			if err := json.NewDecoder(r.Body).Decode(&debit); err != nil || debit.UserID != uid || !strings.HasPrefix(debit.OrderNo, "gift-") {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			call := charges.Add(1)
			switch debit.Point {
			case 2 * price:
				balance.Add(-debit.Point)
				fmt.Fprintf(w, `{"code":200,"data":{"order_id":"%d","order_no":"provider-%d","total_bad_debt_point":0,"total_point":%d}}`, 9007199254741200+call, call, debit.Point)
			case 3 * price:
				fmt.Fprint(w, `{"code":400,"reason":"POINT_NOT_ENOUGH"}`)
			case 4 * price:
				w.WriteHeader(http.StatusBadGateway)
			default:
				w.WriteHeader(http.StatusBadRequest)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(provider.Close)
	path := filepath.Join(t.TempDir(), "gifts.json")
	catalog := fmt.Sprintf(`[{"id":%q,"name":"Flower","price":%d,"icon":"flower.png","incomeStream":7,"performanceRule":1,"performanceTime":1},{"id":"4","name":"Toy","price":3,"effectControlType":2}]`, giftID, price)
	if err := os.WriteFile(path, []byte(catalog), 0600); err != nil {
		t.Fatal(err)
	}
	config.GiftCatalogFile, config.RelaySecret = path, "gift-order-test"
	config.PaymentHost, config.PaymentAppID = provider.URL, "test-payment-app"
	open := func() (*app.App, *httptest.Server) {
		t.Helper()
		platform, err := app.New(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(platform.Close)
		server := httptest.NewServer(platform.Server.Handler)
		t.Cleanup(server.Close)
		return platform, server
	}
	platform, api := open()
	watcherServer := api
	if acrossInstances {
		_, watcherServer = open()
	}
	signUp := func(name string) auth.Session {
		t.Helper()
		s, err := platform.Accounts.SignUp(ctx, name+"@example.test", "password", name)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	host, viewer, outsider := signUp("host"), signUp("viewer"), signUp("outsider")
	room, err := platform.Rooms.Start(ctx, host.User.ID, "Live", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := platform.Rooms.Join(ctx, room.ID, viewer.User.ID, ""); err != nil {
		t.Fatal(err)
	}
	roomID := strconv.FormatInt(room.ID, 10)
	viewerID := strconv.FormatInt(viewer.User.ID, 10)
	type response struct {
		Status, URL, Message string
		Data                 json.RawMessage
		RankSpend, Members   int64
	}
	request := func(server *httptest.Server, method, path string, who auth.Session, key, body string, wantHTTP int, wantStatus string) (response, http.Header) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+who.Token)
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		res, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out response
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != wantHTTP || out.Status != wantStatus {
			t.Fatalf("%s %s = %d status=%q message=%q", method, path, res.StatusCode, out.Status, out.Message)
		}
		return out, res.Header
	}
	points := func(want int64) {
		t.Helper()
		out, _ := request(api, http.MethodGet, "/api/payment/points/get", viewer, "", "", http.StatusOK, "200")
		var data struct{ Points int64 }
		if err := json.Unmarshal(out.Data, &data); err != nil || data.Points != want {
			t.Fatalf("balance = %+v, want %d; err = %v", data, want, err)
		}
	}
	points(startBalance)
	ticket, _ := request(watcherServer, http.MethodGet, "/api/device/qrcode", viewer, "", "", http.StatusOK, "200")
	dialer := websocket.Dialer{Subprotocols: []string{"h5relayer"}, HandshakeTimeout: 5 * time.Second}
	conn, _, err := dialer.DialContext(ctx, ticket.URL+"?room_id="+roomID, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var greeting struct{ Content string }
	if err := conn.ReadJSON(&greeting); err != nil || greeting.Content != "control" {
		t.Fatalf("H5 greeting = %+v, err = %v", greeting, err)
	}
	send := func(who auth.Session, sender, receiver, item string, liveID int64, quantity int, key string, httpStatus int, status string) (response, http.Header) {
		t.Helper()
		const format = `{"date_room_id":%q,"live_id":"%d","gift_id":%q,"limit_id":"0","sender_id":%q,"receiver_id":%q,"quantity":%d,"unit_price":1,"sender_role":5}`
		body := fmt.Sprintf(format, roomID, liveID, item, sender, receiver, quantity)
		return request(api, http.MethodPost, "/api/date/1vN/give/gifts", who, key, body, httpStatus, status)
	}
	liveID := room.Session.LiveID
	otherID := strconv.FormatInt(outsider.User.ID, 10)
	send(outsider, viewerID, roomID, giftID, liveID, 2, "spoof", http.StatusUnauthorized, "401")
	send(outsider, otherID, roomID, giftID, liveID, 2, "non-member", http.StatusForbidden, "403")
	send(viewer, viewerID, otherID, giftID, liveID, 2, "bad-owner", http.StatusForbidden, "403")
	send(viewer, viewerID, roomID, giftID, liveID+1, 2, "invalid-live", http.StatusConflict, "409")
	send(viewer, viewerID, roomID, "4", liveID, 2, "toy", http.StatusUnprocessableEntity, "422")
	if charges.Load() != 0 {
		t.Fatal("an invalid purchase reached the payment provider")
	}
	first, firstHeaders := send(viewer, viewerID, roomID, giftID, liveID, 2, "first-purchase", http.StatusOK, "200")
	if firstHeaders.Get("Idempotency-Key") != "first-purchase" || firstHeaders.Get("X-Payment-Order") == "" {
		t.Fatalf("missing replay identifiers: %v", firstHeaders)
	}
	var initial gift.Result
	if err := json.Unmarshal(first.Data, &initial); err != nil || initial.Quota || len(initial.Success) == 0 || len(initial.Performance) == 0 {
		t.Fatalf("first gift response = %s, error = %v", first.Data, err)
	}
	for _, wantType := range []int{1, 2} {
		var frame struct {
			Content, Queue string
			Payload        []struct {
				ID, Channel, Room, Sender string
				Message                   struct {
					MsgType  int
					MsgGroup string
					Data     json.RawMessage `json:"message"`
				}
			}
		}
		if err := conn.ReadJSON(&frame); err != nil {
			t.Fatal(err)
		}
		if frame.Content != "live_chat" || frame.Queue != roomID || len(frame.Payload) != 1 {
			t.Fatalf("gift frame = %+v", frame)
		}
		p := frame.Payload[0]
		if p.Channel != "realtime" || p.Room != roomID || p.Sender != viewerID || p.Message.MsgGroup != "gift" || p.Message.MsgType != wantType {
			t.Fatalf("gift payload = %+v, want message type %d", p, wantType)
		}
		if wantType == 1 {
			var data struct {
				GiftID       int64 `json:"gift_id"`
				SenderRole   int   `json:"sender_role"`
				AnchorIncome int64
			}
			if err := json.Unmarshal(p.Message.Data, &data); err != nil || data.GiftID != 9007199254740995 || data.SenderRole != 2 || data.AnchorIncome != 7 {
				t.Fatalf("H5 gift details = %+v, error = %v", data, err)
			}
		}
	}
	if acrossInstances {
		api.Close()
		platform.Close()
		_, api = open()
	}
	_, replayHeaders := send(viewer, viewerID, roomID, giftID, liveID, 2, "first-purchase", http.StatusOK, "200")
	if replayHeaders.Get("X-Payment-Order") != firstHeaders.Get("X-Payment-Order") || charges.Load() != 1 {
		t.Fatalf("purchase replay charged again: %v, calls %d", replayHeaders, charges.Load())
	}
	send(viewer, viewerID, roomID, giftID, liveID, 3, "first-purchase", http.StatusConflict, "409")
	send(viewer, viewerID, roomID, giftID, liveID, 3, "decline", http.StatusOK, "4046")
	send(viewer, viewerID, roomID, giftID, liveID, 3, "decline", http.StatusOK, "4046")
	_, uncertain := send(viewer, viewerID, roomID, giftID, liveID, 4, "uncertain", http.StatusConflict, "409")
	_, retried := send(viewer, viewerID, roomID, giftID, liveID, 4, "uncertain", http.StatusConflict, "409")
	if uncertain.Get("X-Payment-Order") == "" || uncertain.Get("X-Payment-Order") != retried.Get("X-Payment-Order") || charges.Load() != 3 {
		t.Fatalf("uncertain payment changed on retry: %v, %v, calls %d", uncertain, retried, charges.Load())
	}
	_, newOrder := send(viewer, viewerID, roomID, giftID, liveID, 2, "", http.StatusOK, "200")
	if newOrder.Get("Idempotency-Key") == "" || newOrder.Get("X-Payment-Order") == firstHeaders.Get("X-Payment-Order") || charges.Load() != 4 {
		t.Fatalf("ordinary unkeyed purchase = %v, calls %d", newOrder, charges.Load())
	}
	points(startBalance - 4*price)
	rank, _ := request(api, http.MethodPost, "/api/live/room/rank/"+roomID, viewer, "", `{"page":0,"pageSize":1}`, http.StatusOK, "200")
	if rank.RankSpend != 14 || rank.Members != 1 {
		t.Fatalf("gift rank = %+v", rank)
	}
	query := "?room_id=" + roomID + "&anchor_id=" + roomID
	request(api, http.MethodGet, "/api/date/anchor/income/log"+query, outsider, "", "", http.StatusForbidden, "403")
	income, _ := request(api, http.MethodGet, "/api/date/anchor/income/log"+query, host, "", "", http.StatusOK, "200")
	var earnings struct {
		List     []gift.Income
		Sum, Yen int64
	}
	if err := json.Unmarshal(income.Data, &earnings); err != nil || earnings.Sum != 14 || earnings.Yen != 220 || len(earnings.List) != 2 {
		t.Fatalf("gift income = %+v, err = %v", earnings, err)
	}
	total, _ := request(api, http.MethodGet, "/api/date/anchor/total/income"+query, host, "", "", http.StatusOK, "200")
	if string(total.Data) != `"14"` {
		t.Fatalf("total income = %s", total.Data)
	}
}
