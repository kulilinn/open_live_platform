package app_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"
	"open_live_platform/internal/admin"
	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/live"
)

func TestPostgresGiftTemplatesAndSharedPurchaseLimits(t *testing.T) {
	cfg, db := postgresTestDatabase(t)
	ctx := t.Context()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	const giftID = "9007199254740993"
	const activity = "9007199254740994"
	const limit = "9007199254740995"
	const box = "9007199254740996"
	const shortcut = "9007199254740997"
	root := t.TempDir()
	cfg.GiftCatalogFile = filepath.Join(root, "catalog.json")
	check(os.WriteFile(cfg.GiftCatalogFile, []byte(`[{"id":"`+giftID+`","name":"Rose","price":40,"incomeStream":10}]`), 0600))
	cfg.AdminAccountsFile, cfg.AdminAddr = filepath.Join(root, "accounts.json"), "127.0.0.1:0"
	cfg.LiveIncomeScheduleToken = "daily-report-test"
	salt := []byte("gift-limit-fixture")
	const password = "test-password"
	key := argon2.IDKey([]byte(password), salt, 2, 4096, 1, 32)
	hash := fmt.Sprintf("$argon2id$v=19$m=4096,t=2,p=1$%s$%s", base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
	users, err := json.Marshal([]admin.Principal{
		{ID: 8, Area: admin.OperationArea, Tenant: admin.OperationTenant, Admin: true, Username: "operations", PasswordHash: hash},
		{ID: 9, Area: admin.OperationArea, Tenant: admin.FinanceTenant, Admin: true, Username: "finance", PasswordHash: hash},
	})
	check(err)
	check(os.WriteFile(cfg.AdminAccountsFile, users, 0600))
	var calls atomic.Int64
	var holdUser, declineUser atomic.Int64
	inPayment, releasePayment := make(chan struct{}), make(chan struct{})
	billing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Points int64 `json:"point"`
			User   int64 `json:"user_id"`
		}
		if r.URL.Path != "/api/payment/point/decrease" || json.NewDecoder(r.Body).Decode(&in) != nil || in.Points != 28 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		call := calls.Add(1)
		if in.User == holdUser.Load() && call == 1 {
			close(inPayment)
			select {
			case <-releasePayment:
			case <-r.Context().Done():
				return
			}
		}
		if in.User == declineUser.Load() {
			fmt.Fprint(w, `{"code":400,"reason":"POINT_NOT_ENOUGH"}`)
			return
		}
		fmt.Fprintf(w, `{"code":200,"data":{"order_id":"%d","order_no":"limit-%d","total_point":28}}`, call, call)
	}))
	t.Cleanup(billing.Close)
	cfg.PaymentHost, cfg.PaymentAppID = billing.URL, "limit-test"
	first, err := app.New(ctx, cfg)
	check(err)
	t.Cleanup(first.Close)
	second, err := app.New(ctx, cfg)
	check(err)
	t.Cleanup(second.Close)
	call := func(handler http.Handler, method, path, token, tenant, body, requestKey string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Loverose-Admin", tenant)
		req.Header.Set("Idempotency-Key", requestKey)
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		return out
	}
	assertReply := func(out *httptest.ResponseRecorder, code int, status string) json.RawMessage {
		t.Helper()
		var result struct {
			Status string
			Data   json.RawMessage
		}
		if err := json.Unmarshal(out.Body.Bytes(), &result); err != nil || out.Code != code || result.Status != status {
			t.Fatalf("got %d/%s %s, want %d/%s, decode %v", out.Code, result.Status, out.Body.String(), code, status, err)
		}
		return result.Data
	}
	login := func(name, tenant string) string {
		t.Helper()
		var session struct{ AccessToken string }
		data := assertReply(call(first.Backoffice.Handler, "POST", "/api/auth/accountlogin", "", tenant,
			fmt.Sprintf(`{"username":%q,"password":%q}`, name, password), ""), 200, "200")
		check(json.Unmarshal(data, &session))
		return session.AccessToken
	}
	operations, finance := login("operations", admin.OperationTenant), login("finance", admin.FinanceTenant)
	manage := func(path, body, status string) json.RawMessage {
		t.Helper()
		return assertReply(call(second.Backoffice.Handler, "POST", "/api/operation/gifts/"+path, operations, "", body, ""), 200, status)
	}
	assertReply(call(second.Backoffice.Handler, "POST", "/api/operation/gifts/activity/list", finance, "", `{}`, ""), 403, "403")
	start := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	end := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	activityBody := fmt.Sprintf(`{"id":"%s","giftId":"%s","name":"Today","description":"limited","startTime":%q,"endTime":%q}`, activity, giftID, start, end)
	manage("activity/add", activityBody, "200")
	manage("gift/discount", `{"users":"987","point":1}`, "200")
	manage("activity/add", activityBody, "4032")
	manage("activity/page", `{"name":"Tod"}`, "200")
	if got := manage("activity/list", `{}`, "200"); !strings.Contains(string(got), activity) {
		t.Fatalf("activity list = %s", got)
	}
	limitBody := fmt.Sprintf(`{"id":"%s","productId":"%s","promotionId":"%s","maxPurchasePerUser":2,"maxPurchasePlatform":2,
		"discountPrice":14,"discountIncome":5,"discountType":1,"discountValue":65,"listingStatus":1,"startTime":%q,"endTime":%q}`,
		limit, giftID, activity, start, end)
	manage("product/add", limitBody, "200")
	if got := manage("product/detail", `{"id":"`+limit+`"}`, "200"); !strings.Contains(string(got), `"discountPrice":14`) {
		t.Fatalf("limit detail = %s", got)
	}
	manage("gift_box/add", `{"id":"`+box+`","name":"Box","giftIds":"`+limit+`"}`, "200")
	manage("gift_shortcut/add", `{"giftId":"`+shortcut+`","limitId":"`+limit+`"}`, "200")
	manage("gift_shortcut/page", `{"giftId":"`+shortcut+`"}`, "200")
	create := func(account string) auth.Session {
		t.Helper()
		session, err := first.Accounts.SignUp(ctx, account+"@gift-limit.test", password, account)
		check(err)
		return session
	}
	owner1, owner2, viewer1, viewer2, viewer3 := create("owner1"), create("owner2"), create("viewer1"), create("viewer2"), create("viewer3")
	room1, err := first.Rooms.Start(ctx, owner1.User.ID, "Room A", "")
	check(err)
	room2, err := second.Rooms.Start(ctx, owner2.User.ID, "Room B", "")
	check(err)
	check(first.Rooms.Join(ctx, room1.ID, viewer1.User.ID, ""))
	check(second.Rooms.Join(ctx, room2.ID, viewer2.User.ID, ""))
	check(second.Rooms.Join(ctx, room2.ID, viewer3.User.ID, ""))
	visible := func(viewer auth.Session, want bool) {
		t.Helper()
		for _, path := range []string{fmt.Sprintf("/api/gift/box/info?box_id=%s&user_id=%d", box, viewer.User.ID),
			fmt.Sprintf("/api/date/gift/quick/list?user_id=%d", viewer.User.ID)} {
			out := assertReply(call(first.Server.Handler, "GET", path, viewer.Token, "", "", ""), 200, "200")
			if strings.Contains(string(out), limit) != want {
				t.Fatalf("%s available=%v, want %v (%s)", path, !want, want, out)
			}
		}
	}
	visible(viewer1, true)
	buy := func(target *app.App, room live.Room, viewer auth.Session, key string) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"date_room_id":"%d","live_id":"%d","gift_id":"%s","limit_id":"%s","sender_id":"%d","receiver_id":"%d","quantity":2}`,
			room.ID, room.Session.LiveID, giftID, limit, viewer.User.ID, room.OwnerID)
		return call(target.Server.Handler, "POST", "/api/date/1vN/give/gifts", viewer.Token, "", body, key)
	}
	holdUser.Store(viewer1.User.ID)
	declineUser.Store(viewer2.User.ID)
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- buy(first, room1, viewer1, "first") }()
	defer func() {
		select {
		case <-releasePayment:
		default:
			close(releasePayment)
		}
	}()
	select {
	case <-inPayment:
	case out := <-result:
		t.Fatalf("first purchase did not reach payment: %s", out.Body.String())
	case <-time.After(3 * time.Second):
		t.Fatal("first purchase did not reach payment")
	}
	assertReply(buy(second, room2, viewer2, "decline"), 200, "4046")
	close(releasePayment)
	assertReply(<-result, 200, "200")
	completed := assertReply(buy(second, room2, viewer3, "last"), 200, "200")
	if !strings.Contains(string(completed), `"quota_flag":true`) {
		t.Fatalf("final allowance was not marked consumed: %s", completed)
	}
	assertReply(buy(first, room2, viewer2, "sold-out"), 200, "4038")
	assertReply(buy(first, room2, viewer3, "last"), 200, "200")
	visible(viewer2, false)
	var paid, income int64
	check(db.QueryRow(ctx, `SELECT count(*),sum(income) FROM gift_orders WHERE state=1 AND purchase_limit_id=$1::text::bigint`, limit).Scan(&paid, &income))
	if calls.Load() != 3 || paid != 2 || income != 10 {
		t.Fatalf("provider calls=%d paid=%d income=%d", calls.Load(), paid, income)
	}
	boundary := time.Now().UTC().Truncate(24 * time.Hour)
	yesterday := boundary.Add(-12 * time.Hour)
	_, err = db.Exec(ctx, `UPDATE room_broadcast_metrics SET started_at=$2,ended_at=$3 WHERE live_id=$1`, room1.Session.LiveID,
		yesterday.Add(-10*time.Minute), yesterday.Add(5*time.Minute))
	check(err)
	_, err = db.Exec(ctx, `UPDATE gift_orders SET paid_at=$2,receipt=receipt||'{"total_bad_debt_point":14}'::jsonb WHERE request_key='first' AND live_id=$1`,
		room1.Session.LiveID, yesterday)
	check(err)
	const collect = "/schedule/statistic/streamer_live_income"
	if out := call(first.Server.Handler, "GET", collect, viewer1.Token, "", "", ""); out.Code != http.StatusForbidden {
		t.Fatalf("public token accepted by the finance schedule: %d %s", out.Code, out.Body.String())
	}
	assertReply(call(first.Server.Handler, "GET", collect, cfg.LiveIncomeScheduleToken, "", "", ""), 200, "200")
	assertReply(call(second.Server.Handler, "GET", collect, cfg.LiveIncomeScheduleToken, "", "", ""), 200, "200")
	const financePath = "/api/finance/purchase/streamer_live_income"
	q := fmt.Sprintf(`{"startTime":%q,"endTime":%q}`, boundary.Add(-24*time.Hour).Format(time.RFC3339), boundary.Add(24*time.Hour).Format(time.RFC3339))
	assertReply(call(second.Backoffice.Handler, "POST", financePath, operations, "", q, ""), 403, "403")
	financeData := assertReply(call(second.Backoffice.Handler, "POST", financePath, finance, "", q, ""), 200, "200")
	for _, field := range []string{fmt.Sprintf(`"streamerUid":"%d"`, owner1.User.ID), `"total":1`, `"diamond":"3"`, `"liveDuration":15`,
		`"created":"` + boundary.Format(time.DateOnly) + `"`} {
		if !strings.Contains(string(financeData), field) {
			t.Fatalf("missing %s in finance snapshot: %s", field, financeData)
		}
	}
	manage("gift_shortcut/empty", `{}`, "200")
	manage("gift_box/delete", `{"id":"`+box+`"}`, "200")
	manage("product/update_status", `{"id":"`+limit+`","status":0}`, "200")
	manage("activity/edit", activityBody, "200")
	manage("activity/delete", `{"id":"`+activity+`"}`, "200")
}
