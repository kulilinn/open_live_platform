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

	"open_live_platform/internal/admin"
	"open_live_platform/internal/app"
	"open_live_platform/internal/payment"
)

func TestPostgresFinancePayments(t *testing.T) {
	config, db := postgresTestDatabase(t)
	ctx := t.Context()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	const staffPassword = "finance-fixture-password"
	hash, err := admin.SystemPassword(staffPassword)
	check(err)
	staff, err := json.Marshal([]admin.Principal{
		{ID: 71, Area: admin.OperationArea, Tenant: admin.FinanceTenant, Username: "finance", Admin: true, PasswordHash: hash},
		{ID: 72, Area: admin.OperationArea, Tenant: admin.FinanceTenant, Username: "finance-read", PasswordHash: hash,
			Permissions: []string{"api:finance:purchase:point:page"}},
		{ID: 73, Area: admin.OperationArea, Tenant: admin.OperationTenant, Username: "ops", Admin: true, PasswordHash: hash},
	})
	check(err)
	const badOrderID int64 = 9007199254741701
	const largeOrderNo = "9007199254741801"
	const giftReceiptID int64 = 9007199254741901
	var userID, balance, credits, debits, debts, large, mails, generic atomic.Int64
	balance.Store(1000)
	started, release := make(chan struct{}), make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := strconv.FormatInt(userID.Load(), 10)
		badRequest := func(message string) {
			t.Errorf("sandbox payment: %s", message)
			http.Error(w, "bad sandbox request", 400)
		}
		respond := func(body string) { _, _ = fmt.Fprint(w, body) }
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/notify" {
			var message struct{ Content string }
			if json.NewDecoder(r.Body).Decode(&message) != nil || !strings.Contains(message.Content, `"template_key":"large-confirm-fixture"`) ||
				!strings.Contains(message.Content, `"description":"`+largeOrderNo+`"`) || !strings.Contains(message.Content, "viewer@example.test") {
				badRequest("invalid confirmation template, recipient or order")
				return
			}
			mails.Add(1)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if r.Header.Get("APPID") != "finance-fixture" || r.Header.Get("Authorization") != "" {
			badRequest("missing payment issuer or leaked backoffice access token")
			return
		}
		if r.URL.Path == "/api/payment/operator/report" || r.URL.Path == "/api/payment/operator/confirm" {
			if r.Header.Get("USER_ID") != "71" || r.Header.Get("BORROW_APPID") != "" {
				badRequest("provider proxy did not use the financial operator identity")
				return
			}
			if r.Method == http.MethodGet {
				respond(`{"report":"upstream"}`)
			} else {
				if !strings.Contains(r.Header.Get("Idempotency-Key"), "general-1") {
					badRequest("proxy omitted the idempotency key")
					return
				}
				generic.Add(1)
				w.WriteHeader(http.StatusCreated)
				respond(`{"provider":"posted"}`)
			}
			return
		}
		if r.Header.Get("USER_ID") != target {
			badRequest("typed provider call did not identify the customer")
			return
		}
		switch r.URL.Path {
		case "/api/payment/user/point":
			respond(fmt.Sprintf(`{"code":200,"data":{"point":%d}}`, balance.Load()))
		case "/api/payment/point/add":
			var debit payment.FinanceCredit
			if json.NewDecoder(r.Body).Decode(&debit) != nil || debit.UserID != userID.Load() || debit.OrderID <= 0 || debit.BadDebtState != 2 || debit.PointExpireDays != 30 {
				badRequest("credit wire contract mismatch")
				return
			}
			credits.Add(1)
			balance.Add(debit.Points)
			if debit.Points == 19 {
				w.WriteHeader(http.StatusBadGateway)
				respond(`{"code":502,"msg":"gateway response lost"}`)
				return
			}
			if debit.Points == 13 {
				close(started)
				<-release
			}
			respond(`{"code":200,"msg":"credited"}`)
		case "/api/payment/point/decrease":
			var debit struct {
				UserID int64  `json:"user_id"`
				Point  int64  `json:"point"`
				Number string `json:"order_no"`
			}
			if json.NewDecoder(r.Body).Decode(&debit) != nil || debit.UserID != userID.Load() || debit.Number == "" {
				badRequest("debit wire contract mismatch")
				return
			}
			debits.Add(1)
			if debit.Point > balance.Load() {
				w.WriteHeader(http.StatusConflict)
				respond(`{"code":409,"reason":"POINT_NOT_ENOUGH"}`)
				return
			}
			balance.Add(-debit.Point)
			id, number := int64(9007199254741902), "finance-points-debit"
			if debit.Point == 30 {
				id, number = giftReceiptID, "finance-gift-receipt"
			}
			respond(fmt.Sprintf(`{"code":200,"data":{"order_id":"%d","order_no":%q,"total_point":%d,"total_bad_debt_point":0}}`, id, number, debit.Point))
		case "/api/payment/large_order/update":
			var update payment.LargeOrderCredit
			if json.NewDecoder(r.Body).Decode(&update) != nil || update.UserID != userID.Load() || update.Number != largeOrderNo ||
				update.TransactionID != "bank-proof" || update.PayAmount != "16.58" || update.OperatorID != 71 {
				badRequest("large-order wire contract mismatch")
				return
			}
			large.Add(1)
			balance.Add(update.Points)
			respond(`{"code":200,"data":{}}`)
		case "/api/payment/order/set_bad_debt":
			var input struct{ ID int64 }
			if json.NewDecoder(r.Body).Decode(&input) != nil || input.ID != badOrderID {
				badRequest("wrong payment order ID")
				return
			}
			debts.Add(1)
			respond(fmt.Sprintf(`{"code":200,"data":{"items":[{"order_id":"%d","order_no":"finance-gift-receipt","total_point":30,"total_bad_debt_point":15}]}}`, giftReceiptID))
		default:
			badRequest("unexpected path " + r.URL.Path)
		}
	}))
	t.Cleanup(provider.Close)
	config.AdminAddr = "127.0.0.1:0"
	dir := t.TempDir()
	config.AdminAccountsFile = filepath.Join(dir, "staff.json")
	config.GiftCatalogFile = filepath.Join(dir, "gifts.json")
	check(os.WriteFile(config.AdminAccountsFile, staff, 0600))
	check(os.WriteFile(config.GiftCatalogFile, []byte(`[{"id":"8102","name":"Finance gift","price":30,"incomeStream":10}]`), 0600))
	config.PaymentHost, config.PaymentAppID = provider.URL, "finance-fixture"
	config.PaymentAdminProxyRoutes = "GET /api/payment/operator/report,POST /api/payment/operator/confirm"
	config.LargeOrders = payment.LargeOrderConfig{NotificationURL: provider.URL + "/notify", FromAddress: "finance@example.test", ConfirmationTemplate: "large-confirm-fixture"}
	start := func() *app.App {
		t.Helper()
		server, err := app.New(ctx, config)
		check(err)
		t.Cleanup(server.Close)
		return server
	}
	a, replica := start(), start()
	host, err := a.Accounts.SignUp(ctx, "anchor-finance@example.test", "creator-password", "Host")
	check(err)
	user, err := a.Accounts.SignUp(ctx, "viewer@example.test", "creator-password", "Viewer")
	check(err)
	userID.Store(user.User.ID)
	type response struct {
		Data   json.RawMessage
		Status string
	}
	call := func(handler http.Handler, method, path, token, tenant, key, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Loverose-Admin", tenant)
		r.Header.Set("Content-Type", "application/json")
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	reply := func(w *httptest.ResponseRecorder, code int, status string) json.RawMessage {
		t.Helper()
		var result response
		check(json.Unmarshal(w.Body.Bytes(), &result))
		if code != w.Code || status != result.Status {
			t.Fatalf("request got %d %s, want %d/%s", w.Code, w.Body.String(), code, status)
		}
		return result.Data
	}
	login := func(account, tenant string) string {
		t.Helper()
		w := call(a.Backoffice.Handler, "POST", "/api/auth/accountlogin", "", tenant, "", fmt.Sprintf(`{"username":%q,"password":%q}`, account, staffPassword))
		var tokens struct{ AccessToken string }
		check(json.Unmarshal(reply(w, 200, "200"), &tokens))
		return tokens.AccessToken
	}
	financeToken, readToken, opsToken := login("finance", admin.FinanceTenant), login("finance-read", admin.FinanceTenant), login("ops", admin.OperationTenant)
	const financePath = "/api/finance/"
	finance := func(handler http.Handler, path, key, body string) *httptest.ResponseRecorder {
		return call(handler, "POST", financePath+path, financeToken, admin.FinanceTenant, key, body)
	}
	addBody := func(points int64) string {
		return fmt.Sprintf(`{"userId":"%d","ty":2,"point":%d,"pointExpireDays":30,"reason":"Points review fixture"}`, user.User.ID, points)
	}
	const add = "purchase/point/add"
	reply(call(a.Backoffice.Handler, "POST", financePath+add, opsToken, admin.OperationTenant, "", addBody(41)), 403, "403")
	reply(call(a.Backoffice.Handler, "POST", financePath+add, readToken, admin.FinanceTenant, "", addBody(41)), 403, "403")
	w := finance(a.Backoffice.Handler, add, "finance-credit-1", addBody(41))
	reply(w, 200, "200")
	id := w.Header().Get("X-Finance-Attempt")
	replay := finance(replica.Backoffice.Handler, add, "finance-credit-1", addBody(41))
	reply(replay, 200, "200")
	if id == "" || id != replay.Header().Get("X-Finance-Attempt") {
		t.Fatalf("credit replay changed its operation ID")
	}
	reply(finance(replica.Backoffice.Handler, add, "finance-credit-1", addBody(49)), 409, "409")
	for _, handler := range []http.Handler{a.Backoffice.Handler, replica.Backoffice.Handler} {
		reply(finance(handler, add, "finance-unknown-credit", addBody(19)), 202, "202")
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- finance(a.Backoffice.Handler, add, "finance-concurrent-credit", addBody(13)) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("provider did not receive first concurrent request")
	}
	inFlight := finance(replica.Backoffice.Handler, add, "finance-concurrent-credit", addBody(13))
	close(release)
	reply(inFlight, 202, "202")
	reply(<-done, 200, "200")
	reply(finance(replica.Backoffice.Handler, add, "finance-concurrent-credit", addBody(13)), 200, "200")
	sub := func(points int64) string {
		return fmt.Sprintf(`{"userId":"%d","point":%d,"reason":"Point debit"}`, user.User.ID, points)
	}
	for _, handler := range []http.Handler{a.Backoffice.Handler, replica.Backoffice.Handler} {
		reply(finance(handler, "purchase/point/sub", "finance-sub-1", sub(39)), 200, "200")
		reply(finance(handler, "purchase/point/sub", "finance-sub-rejected", sub(9999)), 409, "409")
	}
	creditOrder := func(amount string) string {
		return fmt.Sprintf(`{"userId":"%d","orderNo":%q,"transId":"bank-proof","amount":%s,"point":100,"pointExpireDay":10,"remark":"Verified transfer","createBy":72}`, user.User.ID, largeOrderNo, amount)
	}
	reply(finance(a.Backoffice.Handler, "order/large/add_point", "", creditOrder(`16.5800`)), 200, "200")
	reply(finance(replica.Backoffice.Handler, "order/large/add_point", "", creditOrder(`"16.58"`)), 200, "200")
	reply(finance(replica.Backoffice.Handler, "order/large/add_point", "", creditOrder(`17.58`)), 409, "409")
	mailBody := fmt.Sprintf(`{"no":%q,"userId":"%d"}`, largeOrderNo, user.User.ID)
	for _, handler := range []http.Handler{a.Backoffice.Handler, replica.Backoffice.Handler} {
		reply(finance(handler, "order/large/second_send", "finance-mail-1", mailBody), 200, "200")
	}
	reply(finance(a.Backoffice.Handler, "order/large/second_send", "", fmt.Sprintf(`{"no":%q,"userId":"%d"}`, largeOrderNo, host.User.ID)), 200, "4005")
	room, err := a.Rooms.Start(ctx, host.User.ID, "Finance gift room", "")
	check(err)
	check(a.Rooms.Join(ctx, room.ID, user.User.ID, ""))
	buy := fmt.Sprintf(`{"date_room_id":"%d","live_id":"%d","gift_id":"8102","sender_id":"%d","receiver_id":"%d","quantity":1}`,
		room.ID, room.Session.LiveID, user.User.ID, host.User.ID)
	reply(call(a.Server.Handler, "POST", "/api/date/1vN/give/gifts", user.Token, "", "finance-gift", buy), 200, "200")
	debt := fmt.Sprintf(`{"orderId":"%d","userId":"%d"}`, badOrderID, user.User.ID)
	for _, handler := range []http.Handler{a.Backoffice.Handler, replica.Backoffice.Handler} {
		reply(finance(handler, "order/set_bad_debt", "", debt), 200, "200")
	}
	var recorded, paidDebt int64
	check(db.QueryRow(ctx, `SELECT bad_debt_points FROM finance_bad_debt_receipts WHERE provider_order_id=$1`, giftReceiptID).Scan(&recorded))
	check(db.QueryRow(ctx, `SELECT (receipt->>'total_bad_debt_point')::bigint FROM gift_orders WHERE provider_order_id=$1`, giftReceiptID).Scan(&paidDebt))
	if recorded != 15 || paidDebt != 0 {
		t.Fatalf("bad debt replaced original receipt: review %d, receipt %d", recorded, paidDebt)
	}
	if data := reply(finance(a.Backoffice.Handler, "purchase/page", "", `{}`), 200, "200"); !strings.Contains(string(data), `"badDebtPoint":15,"badDebtLd":5`) {
		t.Fatalf("finance purchase report did not reconcile the adjustment: %s", data)
	}
	_, err = db.Exec(ctx, `INSERT INTO legacy_business_history(source_table,source_id,owner_id,record) VALUES
		('admin_modify_point_log','910',71,'{"users":"9007199254741921","point":-2,"create_by_name":"Old operator","create_time":"2026-01-21 00:00:00","reason":"Imported"}'),
		('order_statistic_data','9007199254741922',0,'{"dau":94,"first_purchase_num":2,"purchase_user_num":8,"point_total":4294967297,"consume_point_total":14,"platform_point_total":2,"pay_amount":101.12,"receivable_amount":99.99,"create_time":"2026-01-21"}')`)
	check(err)
	if data := reply(call(a.Backoffice.Handler, "POST", financePath+"purchase/point/page", readToken, admin.FinanceTenant, "", `{}`), 200, "200"); !strings.Contains(string(data), `"total":5`) || strings.Contains(string(data), `"point":19`) {
		t.Fatalf("point page misreported confirmed, legacy or unresolved orders: %s", data)
	}
	if data := reply(finance(a.Backoffice.Handler, "purchase/statistic/today", "", `{"startTime":"2026-01-20","endTime":"2026-01-21"}`), 200, "200"); !strings.Contains(string(data), `"pointTotal":4294967297`) || !strings.Contains(string(data), `"receivableAmount":"99.99"`) {
		t.Fatalf("daily purchase statistics lost original ledger precision: %s", data)
	}
	proxy := func(h http.Handler, method, route, token, key, body string) *httptest.ResponseRecorder {
		return call(h, method, "/api/payment/operator/"+route, token, admin.FinanceTenant, key, body)
	}
	reply(proxy(a.Backoffice.Handler, "GET", "report", opsToken, "", ""), 403, "403")
	reply(proxy(a.Backoffice.Handler, "GET", "no_route", financeToken, "", ""), 404, "404")
	if w := proxy(a.Backoffice.Handler, "GET", "report", financeToken, "", ""); w.Code != 200 || w.Body.String() != `{"report":"upstream"}` {
		t.Fatalf("unexpected private payment report: %d %s", w.Code, w.Body.String())
	}
	for _, handler := range []http.Handler{a.Backoffice.Handler, replica.Backoffice.Handler} {
		if w := proxy(handler, "POST", "confirm", financeToken, "general-1", `{"status":"ok"}`); w.Code != 201 || w.Body.String() != `{"provider":"posted"}` {
			t.Fatalf("unexpected private payment POST or replay: %d %s", w.Code, w.Body.String())
		}
	}
	reply(proxy(a.Backoffice.Handler, "POST", "confirm", financeToken, "general-1", `{"status":"different"}`), 409, "409")
	if calls := []int64{credits.Load(), debits.Load(), debts.Load(), large.Load(), mails.Load(), generic.Load(), balance.Load()}; fmt.Sprint(calls) != "[3 3 1 1 1 1 1104]" {
		t.Fatalf("payment endpoints executed more or fewer times than expected: %v", calls)
	}
	var uncertain int64
	check(db.QueryRow(ctx, `SELECT count(*) FROM finance_payment_attempts WHERE action='point_add' AND state='unknown' AND point_delta=19`).Scan(&uncertain))
	if uncertain != 1 {
		t.Fatalf("ambiguous credit was not held for reconciliation")
	}
}
