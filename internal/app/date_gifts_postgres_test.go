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
	"open_live_platform/internal/store/postgres"
)

func TestPostgresDateGiftsPaymentPermissionAndQueue(t *testing.T) {
	cfg, db := postgresTestDatabase(t)
	ctx := t.Context()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var payments atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("APPID") != "dategift" {
			http.Error(w, "invalid app", http.StatusForbidden)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/payment/user/point":
			fmt.Fprint(w, `{"code":200,"data":{"point":900}}`)
		case "POST /api/payment/point/decrease":
			var input struct {
				Point   int64
				OrderNo string `json:"order_no"`
			}
			if json.NewDecoder(r.Body).Decode(&input) != nil || !strings.HasPrefix(input.OrderNo, "date-gift-") {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			id := payments.Add(1)
			fmt.Fprintf(w, `{"code":200,"data":{"order_id":"%d","order_no":"order-%d","total_point":%d}}`, id, id, input.Point)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(provider.Close)
	cfg.PaymentHost, cfg.PaymentAppID = provider.URL, "dategift"
	cfg.GiftCatalogFile = filepath.Join(t.TempDir(), "catalog.json")
	check(os.WriteFile(cfg.GiftCatalogFile, []byte(`[
		{"id":"501","price":20,"name":"Flower"},
		{"id":"502","price":30,"name":"Game","giftType":1,"effectControlType":1},
		{"id":"503","price":40,"name":"Toy","giftType":2,"effectControlType":2,"parameterOne":"12","parameterTwo":10}
	]`), 0600))
	open := func() *app.App {
		platform, err := app.New(ctx, cfg)
		check(err)
		t.Cleanup(platform.Close)
		return platform
	}
	first, second := open(), open()
	signUp := func(name string) auth.Session {
		v, err := first.Accounts.SignUp(ctx, name+"@dategift.test", "password", name)
		check(err)
		return v
	}
	anchor, fan, other := signUp("anchor"), signUp("fan"), signUp("other")
	now := time.Now()
	session := date.Session{Channel: date.NewChannel(), AnchorID: anchor.User.ID, AudienceID: fan.User.ID,
		StartedAt: now, AnchorSeen: now.Add(time.Minute), AudienceSeen: now.Add(time.Minute)}
	_, err := db.Exec(ctx, `INSERT INTO date_sessions(channel,anchor_id,audience_id,state) VALUES($1,$2,$3,$4)`,
		session.Channel, session.AnchorID, session.AudienceID, session)
	check(err)
	channel := strconv.FormatInt(session.Channel, 10)
	request := func(a *app.App, who auth.Session, method, path, key, body string, httpCode int, status string) json.RawMessage {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+who.Token)
		r.Header.Set("Content-Type", "application/json")
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		w := httptest.NewRecorder()
		a.Server.Handler.ServeHTTP(w, r)
		var response struct {
			Status string
			Data   json.RawMessage
		}
		check(json.NewDecoder(w.Body).Decode(&response))
		if w.Code != httpCode || response.Status != status {
			t.Fatalf("%s %s = %d status=%s, want %d/%s", method, path, w.Code, response.Status, httpCode, status)
		}
		return response.Data
	}
	send := func(platform *app.App, who auth.Session, giftID, quantity int, key string, httpCode int, status string) {
		t.Helper()
		request(platform, who, "POST", "/api/date/give/gifts", key,
			fmt.Sprintf(`{"dateRoomId":%q,"giftId":"%d","senderId":"%d","receiverId":"%d","quantity":%d,"unitPrice":1}`,
				channel, giftID, fan.User.ID, anchor.User.ID, quantity), httpCode, status)
	}
	send(first, other, 501, 1, "bad-actor", 403, "403")
	request(second, other, "GET", "/api/date/gifts/transaction/"+channel, "", "", 403, "403")
	request(second, fan, "GET", "/api/date/gifts/"+channel, "", "", 200, "200")
	send(first, fan, 501, 2, "ordinary", 200, "200")
	send(second, fan, 501, 2, "ordinary", 200, "200")
	send(second, fan, 501, 1, "ordinary", 409, "409")
	send(first, fan, 502, 1, "game", 200, "200")
	send(second, fan, 502, 1, "second-game", 200, "4028")
	if payments.Load() != 1 {
		t.Fatalf("charges before game acceptance = %d", payments.Load())
	}
	raw := request(second, anchor, "GET", "/api/gift/requests/"+channel, "", "", 200, "200")
	var pending [][]json.RawMessage
	check(json.Unmarshal(raw, &pending))
	if len(pending) != 1 || len(pending[0]) != 2 {
		t.Fatalf("game pending data = %s", raw)
	}
	body := fmt.Sprintf(`{"channel":%q,"gift":%s,"accept":true}`, channel, pending[0][0])
	request(first, fan, "POST", "/api/gift/accept", "", body, 403, "403")
	request(second, anchor, "POST", "/api/gift/accept", "", body, 200, "200")
	request(first, anchor, "POST", "/api/gift/accept", "", body, 200, "200")
	send(second, fan, 503, 2, "toy", 200, "200")
	queue := request(first, anchor, "GET", "/api/date/gift/list/executing/"+channel, "", "", 200, "200")
	var items []struct {
		ID, Name      string
		OperationTime int64
		EndTime       int64
	}
	check(json.Unmarshal(queue, &items))
	if len(items) != 2 || items[0].ID == items[1].ID || items[0].Name != "Toy" ||
		items[0].OperationTime <= 0 || items[1].OperationTime != 12 || items[1].EndTime-items[0].EndTime != 12 {
		t.Fatalf("hardware queue = %s", queue)
	}
	entries, err := second.Rooms.(*postgres.Store).ClaimDateGiftStarts(ctx)
	check(err)
	if len(entries) != 1 || strconv.FormatInt(entries[0].ID, 10) != items[0].ID {
		t.Fatalf("claim executing hardware gift %+v", entries)
	}
	entries, err = first.Rooms.(*postgres.Store).ClaimDateGiftStarts(ctx)
	check(err)
	if len(entries) != 0 {
		t.Fatal("hardware gift announced twice by separate instances")
	}
	var points int64
	check(db.QueryRow(ctx, `SELECT sum((order_data->>'Income')::bigint) FROM date_gift_orders WHERE channel=$1 AND state=1`, session.Channel).Scan(&points))
	if points != 150 || payments.Load() != 3 {
		t.Fatalf("paid ledger = %d points and %d calls", points, payments.Load())
	}
	raw = request(first, fan, "GET", "/api/date/gifts/transaction/"+channel, "", "", 200, "200")
	var records []json.RawMessage
	check(json.Unmarshal(raw, &records))
	if len(records) != 3 {
		t.Fatalf("transactions = %s", raw)
	}
	session.EndedAt = time.Now()
	_, err = db.Exec(ctx, `UPDATE date_sessions SET state=$2,ended_at=$3 WHERE channel=$1`, session.Channel, session, session.EndedAt)
	check(err)
	raw = request(second, anchor, "GET", "/api/date/gift/executing/"+channel, "", "", 200, "200")
	if string(raw) != "null" {
		t.Fatalf("closed session shows executing gift: %s", raw)
	}
	send(first, fan, 501, 1, "closed", 200, "4022")
}
