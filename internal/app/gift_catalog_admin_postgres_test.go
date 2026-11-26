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

	"golang.org/x/crypto/argon2"
	"open_live_platform/internal/admin"
	"open_live_platform/internal/app"
)

func TestPostgresOperationalGiftCatalog(t *testing.T) {
	config, db := postgresTestDatabase(t)
	ctx := t.Context()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	const password = "test-gift-catalog"
	salt := []byte("test-salt-asdf12")
	hash := fmt.Sprintf("$argon2id$v=19$m=4096,t=2,p=1$%s$%s", base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(argon2.IDKey([]byte(password), salt, 2, 4096, 1, 32)))
	data, err := json.Marshal([]admin.Principal{
		{ID: 47, Area: admin.OperationArea, Tenant: admin.OperationTenant, Username: "catalog-admin", Admin: true, PasswordHash: hash},
		{ID: 48, Area: admin.OperationArea, Tenant: admin.OperationTenant, Username: "catalog-reader", PasswordHash: hash,
			Permissions: []string{"api:operation:gifts:gift:detail"}},
	})
	check(err)
	config.AdminAddr = "127.0.0.1:0"
	var calls, points atomic.Int64
	billing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var debit struct {
			Point int64
		}
		if r.Method != "POST" || r.URL.Path != "/api/payment/point/decrease" || json.NewDecoder(r.Body).Decode(&debit) != nil {
			http.Error(w, "unexpected payment call", 400)
			return
		}
		call := calls.Add(1)
		points.Add(debit.Point)
		fmt.Fprintf(w, `{"code":200,"data":{"order_id":"%d","order_no":"purchase-%d","total_point":%d}}`, call, call, debit.Point)
	}))
	t.Cleanup(billing.Close)
	config.PaymentHost, config.PaymentAppID = billing.URL, "catalog-test"
	dir := t.TempDir()
	config.AdminAccountsFile = filepath.Join(dir, "operators.json")
	config.GiftCatalogFile = filepath.Join(dir, "gifts.json")
	check(os.WriteFile(config.AdminAccountsFile, data, 0600))
	check(os.WriteFile(config.GiftCatalogFile, []byte(`[{"id":"8001","price":15,"name":"Previous"}]`), 0600))
	start := func() *app.App {
		t.Helper()
		p, err := app.New(ctx, config)
		check(err)
		t.Cleanup(p.Close)
		return p
	}
	ops, platform := start(), start()
	host, err := platform.Accounts.SignUp(ctx, "gift-host@example.test", "password", "Host")
	check(err)
	fan, err := platform.Accounts.SignUp(ctx, "gift-fan@example.test", "password", "Fan")
	check(err)
	room, err := platform.Rooms.Start(ctx, host.User.ID, "Live", "")
	check(err)
	check(platform.Rooms.Join(ctx, room.ID, fan.User.ID, ""))
	request := func(server http.Handler, method, path, token, key, body string, code int, status string) json.RawMessage {
		t.Helper()
		r := httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Loverose-Admin", admin.OperationTenant)
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		var result struct {
			Status string
			Data   json.RawMessage
		}
		check(json.Unmarshal(w.Body.Bytes(), &result))
		if w.Code != code || result.Status != status {
			t.Fatalf("%s = %d %s, expected %d %s", path, w.Code, w.Body.String(), code, status)
		}
		return result.Data
	}
	login := func(name string) string {
		t.Helper()
		out := request(ops.Backoffice.Handler, "POST", "/api/auth/accountlogin", "", "", fmt.Sprintf(`{"username":%q,"password":%q}`, name, password), 200, "200")
		var credentials struct{ AccessToken string }
		check(json.Unmarshal(out, &credentials))
		return credentials.AccessToken
	}
	operator, reader := login("catalog-admin"), login("catalog-reader")
	update := func(method, token, body string, code int, status string) json.RawMessage {
		return request(ops.Backoffice.Handler, "POST", "/api/operation/gifts/gift/"+method, token, "", body, code, status)
	}
	update("edit", reader, `{"id":"8001","name":"Spoof","price":9}`, 403, "403")
	update("edit", operator, `{"id":"8001","name":"Updated gift","price":"25.00","incomeStream":4}`, 200, "200")
	update("add", operator, `{"id":"8002","name":"Added gift","price":10}`, 200, "200")
	update("add", operator, `{"id":"8002","name":"Duplicate","price":10}`, 409, "409")
	if detail := update("detail", reader, `{"id":"8001"}`, 200, "200"); !strings.Contains(string(detail), `"price":"25"`) {
		t.Fatalf("backoffice detail = %s", detail)
	}
	read := func() json.RawMessage {
		return request(platform.Server.Handler, "GET", "/api/gift/list", fan.Token, "", "", 200, "200")
	}
	if items := read(); !strings.Contains(string(items), "Updated gift") || !strings.Contains(string(items), "Added gift") ||
		strings.Contains(string(items), "Previous") {
		t.Fatalf("public dynamic catalog = %s", items)
	}
	buy := func(key string, code int, status string) {
		t.Helper()
		body := fmt.Sprintf(`{"date_room_id":"%d","live_id":"%d","sender_id":"%d","receiver_id":"%d","gift_id":"8001","quantity":1}`,
			room.ID, room.Session.LiveID, fan.User.ID, host.User.ID)
		request(platform.Server.Handler, "POST", "/api/date/1vN/give/gifts", fan.Token, key, body, code, status)
	}
	buy("purchase-current", 200, "200")
	if calls.Load() != 1 || points.Load() != 25 {
		t.Fatalf("payment did not get catalog price: %d calls, %d points", calls.Load(), points.Load())
	}
	_, err = db.Exec(ctx, `INSERT INTO legacy_business_history(source_table,source_id,owner_id,record) VALUES
		('pay_orders','9007199254741221',$1,'{"amount":16.58,"point":90,"payment_method_name":"Card","status":2,"create_time":"2026-01-21 04:20:00"}'),
		('point_transactions','1',$1,'{"no":"purchase-1","transaction_type":2,"total_point":25,"income_or_expenses":-1,"create_time":"2026-01-21 04:21:00"}'),
		('point_transactions','9007199254741222',$1,'{"no":"legacy-spend","transaction_type":2,"total_point":45,"income_or_expenses":-1,"transaction_order_id":77,"create_time":"2026-01-21 04:22:00"}'),
		('point_transactions','9007199254741223',$1,'{"no":"legacy-income","transaction_type":2,"total_point":99,"income_or_expenses":1,"create_time":"2026-01-21 04:23:00"}'),
		('gift_production_marketing','77',$1,'{"gift_id":78}'),('gifts','78',78,'{"name":"Legacy gift"}')`, fan.User.ID)
	check(err)
	const reports = "/api/operation/usermanage/user/"
	charge := fmt.Sprintf(`{"uid":"%d","status":2,"startTime":"2026-01-21","endTime":"2026-01-22"}`, fan.User.ID)
	data = request(ops.Backoffice.Handler, "POST", reports+"charge_records", operator, "", charge, 200, "200")
	if !strings.Contains(string(data), `"orderId":"9007199254741221"`) || !strings.Contains(string(data), `"price":"16.58"`) || !strings.Contains(string(data), `"total":1`) {
		t.Fatalf("charge history = %s", data)
	}
	request(ops.Backoffice.Handler, "POST", reports+"charge_records", reader, "", charge, 403, "403")
	data = request(ops.Backoffice.Handler, "POST", reports+"consume_records", operator, "", fmt.Sprintf(`{"uid":"%d","ty":2,"pageSize":2}`, fan.User.ID), 200, "200")
	for _, want := range []string{`"total":2`, `"lp":25`, `"lp":45`, `"Legacy gift"`, `"Updated gift"`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("consume history %s does not contain %s", data, want)
		}
	}
	var post, comment int64
	check(db.QueryRow(ctx, `INSERT INTO anchor_posts(owner_id,content,visibility,price,media,only_text,images,videos,video_length,reference_post,status,comment_count)
		VALUES($1,'What the moderator sees',0,0,'[]',1,0,0,0,'',3,2) RETURNING id`, host.User.ID).Scan(&post))
	check(db.QueryRow(ctx, `INSERT INTO anchor_post_comments(post_id,author_id,content) VALUES($1,$2,'Original comment') RETURNING id`, post, fan.User.ID).Scan(&comment))
	_, err = db.Exec(ctx, `INSERT INTO anchor_post_comments(post_id,author_id,content,parent_id) VALUES($1,$2,'Reply',$3)`, post, host.User.ID, comment)
	check(err)
	const review = "/api/operation/business/post/"
	postID := fmt.Sprintf(`{"id":"%d","uid":"%d"}`, post, host.User.ID)
	comments := fmt.Sprintf(`{"postId":"%d"}`, post)
	commentID := fmt.Sprintf(`{"id":"%d","postId":"%d"}`, comment, post)
	request(ops.Backoffice.Handler, "POST", review+"delete", reader, "", postID, 403, "403")
	data = request(ops.Backoffice.Handler, "POST", review+"page", operator, "", fmt.Sprintf(`{"authorId":"%d","passStatus":3}`, host.User.ID), 200, "200")
	if !strings.Contains(string(data), `"total":1`) || !strings.Contains(string(data), `"commentNum":2`) {
		t.Fatalf("post review page = %s", data)
	}
	data = request(ops.Backoffice.Handler, "POST", review+"detail", operator, "", postID, 200, "200")
	if !strings.Contains(string(data), `"content":"What the moderator sees"`) {
		t.Fatalf("post review detail = %s", data)
	}
	data = request(ops.Backoffice.Handler, "POST", review+"comments", operator, "", comments, 200, "200")
	if !strings.Contains(string(data), `"comments":[`) || !strings.Contains(string(data), "Original comment") || !strings.Contains(string(data), "Reply") {
		t.Fatalf("post review comments = %s", data)
	}
	request(ops.Backoffice.Handler, "POST", review+"detail", operator, "", fmt.Sprintf(`{"id":"%d","uid":"%d"}`, post, fan.User.ID), 200, "4005")
	for range 2 {
		request(ops.Backoffice.Handler, "POST", review+"comment_delete", operator, "", commentID, 200, "200")
	}
	data = request(ops.Backoffice.Handler, "POST", review+"comments", operator, "", comments, 200, "200")
	if !strings.Contains(string(data), `"comments":[]`) {
		t.Fatalf("moderator left comment branch active: %s", data)
	}
	for range 2 {
		request(ops.Backoffice.Handler, "POST", review+"delete", operator, "", postID, 200, "200")
	}
	data = request(platform.Server.Handler, "GET", fmt.Sprintf("/anchor/api/get_one_post/%d", post), fan.Token, "", "", 200, "200")
	if string(data) != "null" {
		t.Fatalf("removed post still appears in public details: %s", data)
	}
	var removes, commentCount int
	check(db.QueryRow(ctx, `SELECT count(*) FROM backoffice_logs WHERE operator_id=47 AND action IN ('post_delete','post_comment_delete')`).Scan(&removes))
	check(db.QueryRow(ctx, `SELECT comment_count FROM anchor_posts WHERE id=$1 AND deleted`, post).Scan(&commentCount))
	if removes != 2 || commentCount != 0 {
		t.Fatalf("post moderator mutations audited %d times, %d active comments", removes, commentCount)
	}
	update("delete", operator, `{"id":"8001"}`, 200, "200")
	if items := read(); strings.Contains(string(items), `"8001"`) {
		t.Fatalf("deleted gift returned by catalog: %s", items)
	}
	buy("purchase-retired", 200, "4005")
	buy("purchase-current", 200, "200")
	if calls.Load() != 1 {
		t.Fatalf("replay or retired item triggered external payment: %d", calls.Load())
	}
	var audited int
	check(db.QueryRow(ctx, `SELECT count(*) FROM backoffice_logs WHERE operator_id=47 AND action IN ('gift_add','gift_edit','gift_delete')`).Scan(&audited))
	if audited != 3 {
		t.Fatalf("successful gift changes audited = %d", audited)
	}
}
