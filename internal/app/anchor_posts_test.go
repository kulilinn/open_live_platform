package app_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
)

func TestAnchorPosts(t *testing.T) {
	t.Run("memory", func(t *testing.T) { testAnchorPosts(t, app.Config{}) })
	t.Run("postgres", func(t *testing.T) {
		config, _ := postgresTestDatabase(t)
		testAnchorPosts(t, config)
	})
}

func testAnchorPosts(t *testing.T, config app.Config) {
	t.Helper()
	var audits atomic.Int32
	audit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var review struct {
			Kind, Text string
			PostID     string `json:"post_id"`
		}
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer external-audit" ||
			json.NewDecoder(r.Body).Decode(&review) != nil ||
			(review.Kind != "post" && review.Kind != "comment" && review.Kind != "public_chat") {
			t.Error("unexpected moderation request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		audits.Add(1)
		status := 3
		switch review.Text {
		case "waiting":
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		case "queued":
			status = 1
		case "reject":
			status = 2
		}
		fmt.Fprintf(w, `{"pass_status":%d,"request_id":%q,"manual_request_id":%q}`, status,
			"machine-"+review.PostID, "manual-"+review.PostID)
	}))
	defer audit.Close()
	config.PostAuditURL, config.PostAuditToken = audit.URL, "external-audit"
	config.PostReviewToken = "trusted-backoffice"
	platform, err := app.New(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(platform.Close)
	owner := subscriptionTestUser(t, platform, "post-owner", true)
	fan := subscriptionTestUser(t, platform, "post-fan", false)
	outsider := subscriptionTestUser(t, platform, "post-other", false)
	anchor := strconv.FormatInt(owner.User.ID, 10)
	request := func(method, route string, account auth.Session, body any, code int) map[string]any {
		t.Helper()
		return anchorPostRequest(t, platform, method, route, account.Token, "", body, code)
	}
	create := func(account auth.Session, content string, level int, preview bool, code int) string {
		t.Helper()
		onlyText := 0
		if preview {
			onlyText = 1
		}
		data := request("POST", "/anchor/api/create_post", account, map[string]any{
			"content": content, "level": level, "only_text": onlyText, "img_num": 1, "video_num": 1, "video_length": 4,
			"url": []map[string]any{{"url": "https://media.invalid/original.mp4", "filename": "poster", "id": "upload",
				"blur_url": "/objects/blur.webp", "cover_img": "https://media.invalid/cover.jpg", "video_length": 4}},
			"reference_post": "private-reference",
		}, code)
		if code != http.StatusOK {
			return ""
		}
		return strconv.FormatInt(int64(data["result"].(map[string]any)["post_id"].(float64)), 10)
	}
	detail := func(account auth.Session, id string) map[string]any {
		t.Helper()
		res := request("GET", "/anchor/api/get_one_post/"+id, account, nil, http.StatusOK)
		view, _ := res["data"].(map[string]any)
		return view
	}
	list := func(account auth.Session, options map[string]any) []any {
		t.Helper()
		if options == nil {
			options = map[string]any{}
		}
		options["anchor_id"], options["page_size"] = anchor, 10
		return request("POST", "/anchor/api/get_post", account, options, http.StatusOK)["data"].([]any)
	}
	create(outsider, "not an anchor", 0, false, http.StatusForbidden)
	private := create(owner, "private", 1, false, http.StatusOK)
	limited := create(owner, "subscriber preview", 2, true, http.StatusOK)
	basic := create(owner, "basic-only", 3, false, http.StatusOK)
	approved := create(owner, "public", 0, false, http.StatusOK)
	pending := create(owner, "waiting", 0, false, http.StatusOK)
	rejected := create(owner, "reject", 0, false, http.StatusOK)
	queued := create(owner, "queued", 0, false, http.StatusOK)
	if detail(fan, private) != nil || detail(fan, pending) != nil || detail(fan, rejected) != nil || detail(fan, queued) != nil ||
		detail(owner, pending)["pass_status"] != float64(1) || detail(owner, rejected)["pass_status"] != float64(2) ||
		len(list(fan, nil)) != 3 || len(list(owner, map[string]any{"only_self": 1})) != 1 {
		t.Fatal("private and unapproved content leaked or disappeared from the owner's view")
	}
	review := map[string]any{"post_id": queued, "pass_status": 3}
	request("POST", "/internal/anchor-post/review", fan, review, http.StatusUnauthorized)
	status := anchorPostRequest(t, platform, "POST", "/internal/anchor-post/review", config.PostReviewToken, "", review, http.StatusOK)
	if status["status"] != "200" || detail(fan, queued)["pass_status"] != float64(3) {
		t.Fatalf("review did not publish the queued post: %+v", status)
	}
	locked := detail(fan, limited)
	if locked["privilege"] != float64(0) || locked["content"] != "subscriber preview" || locked["reference_post"] != "" ||
		strings.Contains(locked["url"].(string), "media.invalid") || !strings.Contains(locked["url"].(string), "/objects/blur.webp") {
		t.Fatalf("invalid locked view: %+v", locked)
	}
	if len(list(fan, map[string]any{"search_content": "basic-only"})) != 0 {
		t.Fatal("search reveals content from a locked post")
	}
	request("GET", "/anchor/api/get_post_comment?post_id="+limited, fan, nil, http.StatusForbidden)
	comment := func(account auth.Session, post, content string, parent any, code int) map[string]any {
		t.Helper()
		return request("POST", "/anchor/api/comment_post", account, map[string]any{
			"post_id": post, "content": content, "parent_comment_id": parent,
		}, code)
	}
	before := audits.Load()
	comment(fan, limited, "not eligible", nil, http.StatusForbidden)
	if audits.Load() != before {
		t.Fatal("locked comments reached the audit provider")
	}
	plans := request("POST", "/api/sub/get_anchor_plan", fan, map[string]any{"anchor_id": anchor}, http.StatusOK)["data"].([]any)
	free := plans[0].(map[string]any)["id"].(string)
	request("POST", "/api/sub/free_sub", fan, map[string]any{"anchor_id": anchor, "plan_id": "0"}, http.StatusOK)
	unlocked := detail(fan, limited)
	if unlocked["privilege"] != float64(1) || unlocked["reference_post"] != "private-reference" ||
		!strings.Contains(unlocked["url"].(string), "original.mp4") || !strings.Contains(unlocked["url"].(string), "cover.jpg") ||
		detail(fan, basic)["privilege"] != float64(0) {
		t.Fatalf("invalid subscription view: %+v", unlocked)
	}
	if res := comment(fan, limited, "reject", nil, http.StatusOK); res["status"] != "4060" {
		t.Fatalf("comment audit status: %+v", res)
	}
	comment(fan, limited, "reply here", nil, http.StatusOK)
	comments := func() []any {
		t.Helper()
		return request("GET", "/anchor/api/get_post_comment?post_id="+limited, fan, nil, http.StatusOK)["data"].([]any)
	}
	root := comments()[0].(map[string]any)["id"].(string)
	if res := comment(owner, approved, "wrong reply target", root, http.StatusOK); res["status"] != "4005" {
		t.Fatalf("cross-post parent accepted: %+v", res)
	}
	comment(owner, limited, "nested response", root, http.StatusOK)
	request("GET", "/anchor/api/del_comment?comment_id="+root, outsider, nil, http.StatusForbidden)
	for range 2 {
		for _, like := range []struct{ slug, key, id string }{{"like_post", "post_id", limited}, {"like_comment", "comment_id", root}} {
			result := request("POST", "/anchor/api/"+like.slug, fan, map[string]any{like.key: like.id, "is_like": true}, http.StatusOK)
			if result["data"].(map[string]any)["count"] != float64(1) {
				t.Fatalf("duplicate like changed count: %+v", result)
			}
		}
	}
	roots := comments()
	entry := roots[0].(map[string]any)
	if len(roots) != 1 || len(entry["children"].([]any)) != 1 || entry["likes"] != float64(1) || entry["liked"] != float64(1) {
		t.Fatalf("comment tree = %+v", roots)
	}
	reactions := request("GET", "/anchor/api/get_post_like?post_id="+limited+"&page=1&page_size=20", owner, nil, http.StatusOK)
	if len(reactions["data"].([]any)) != 1 {
		t.Fatalf("like list = %+v", reactions)
	}
	request("POST", "/anchor/api/like_post", fan, map[string]any{"post_id": limited, "is_like": false}, http.StatusOK)
	request("GET", "/anchor/api/del_comment?comment_id="+root, owner, nil, http.StatusOK)
	if len(comments()) != 0 || detail(fan, limited)["comment_num"] != float64(0) || detail(fan, limited)["like_num"] != float64(0) {
		t.Fatal("comment subtree or like count did not update")
	}
	page := request("POST", "/anchor/api/page", fan, map[string]string{"anchor_id": anchor}, http.StatusOK)["data"].(map[string]any)
	recent := request("POST", "/anchor/api/recent_update", fan, nil, http.StatusOK)["data"].([]any)
	search := request("POST", "/anchor/api/search", fan, map[string]string{"username": "post-owner"}, http.StatusOK)["data"].([]any)
	if page["post_num"] != float64(4) || page["sub_num"] != float64(1) || len(recent) != 1 || len(search) != 1 {
		t.Fatalf("anchor discovery page=%+v recent=%+v search=%+v", page, recent, search)
	}
	published := create(owner, "queued", 2, false, http.StatusOK)
	create(owner, "unlicensed for free subscribers", 3, false, http.StatusOK)
	create(owner, "for author only", 1, false, http.StatusOK)
	reviewMedia := func(route, token, actor, risk, requestID string, expected int) map[string]any {
		t.Helper()
		return anchorPostRequest(t, platform, "POST", route, token, "", map[string]any{
			"requestId": requestID, "riskLevel": risk, "passThrough": map[string]string{"userId": actor, "dataId": published},
		}, expected)
	}
	reviewMedia("/sm/media/callback", fan.Token, anchor, "PASS", "machine-"+published, http.StatusUnauthorized)
	reviewMedia("/sm/media/callback", config.PostReviewToken, strconv.FormatInt(fan.User.ID, 10), "PASS", "machine-"+published, http.StatusNotFound)
	reviewMedia("/sm/media/callback", config.PostReviewToken, anchor, "PASS", "another-"+published, http.StatusNotFound)
	if result := reviewMedia("/sm/media/callback", config.PostReviewToken, anchor, "REVIEW", "machine-"+published, http.StatusOK); result["code"] != float64(1100) ||
		detail(fan, published) != nil {
		t.Fatalf("machine review unexpectedly published the post: %+v", result)
	}
	for range 2 {
		reviewMedia("/sm/media_manual/callback", config.PostReviewToken, anchor, "PASS", "manual-"+published, http.StatusOK)
	}
	noticeRows := func(account auth.Session) []any {
		t.Helper()
		return request("GET", "/api/notice/page_notice?page=1&pageSize=20", account, nil, http.StatusOK)["data"].(map[string]any)["list"].([]any)
	}
	if rows := noticeRows(fan); len(rows) != 1 || rows[0].(map[string]any)["noticeType"] != float64(2) ||
		rows[0].(map[string]any)["objectId"] != published {
		t.Fatalf("subscriber notices = %+v", rows)
	}
	if rows := noticeRows(owner); len(rows) != 3 || rows[0].(map[string]any)["noticeType"] != float64(7) ||
		rows[1].(map[string]any)["noticeType"] != float64(8) || rows[2].(map[string]any)["noticeType"] != float64(17) {
		t.Fatalf("post interaction notices = %+v", rows)
	}
	if rows := noticeRows(outsider); len(rows) != 0 {
		t.Fatalf("outsider received notices: %+v", rows)
	}
	if result := request("POST", "/sm/public_chat/audit", fan, map[string]string{"content": "reject"}, http.StatusOK); result["status"] != "4068" {
		t.Fatalf("public chat review = %+v", result)
	}
	comment(fan, limited, "manual review target", nil, http.StatusOK)
	commentID := comments()[0].(map[string]any)["id"].(string)
	static := func(content string) {
		t.Helper()
		callback := map[string]any{"requestId": "manual-" + limited, "result": map[string]int{"operation": 2}, "data": map[string]any{"passThrough": map[string]any{
			"id": commentID, "userId": strconv.FormatInt(fan.User.ID, 10), "contentType": 12, "content": content}}}
		anchorPostRequest(t, platform, "POST", "/sm/static/callback", config.PostReviewToken, "", callback, http.StatusOK)
	}
	static("stale content")
	if len(comments()) != 1 {
		t.Fatal("static audit removed a different comment")
	}
	for range 2 {
		static("manual review target")
	}
	if len(comments()) != 0 {
		t.Fatal("manual rejection kept the comment visible")
	}
	if rows := noticeRows(fan); len(rows) != 2 || rows[0].(map[string]any)["noticeType"] != float64(30) {
		t.Fatalf("manual review notices = %+v", rows)
	}
	request("POST", "/api/sub/add_group", fan, map[string]any{"uid": anchor, "ty": 3}, http.StatusOK)
	create(owner, "blocked anchor", 0, false, http.StatusOK)
	if rows := noticeRows(fan); len(rows) != 2 {
		t.Fatalf("blocked anchor sent unexpected notices: %+v", rows)
	}
	request("GET", "/api/sub/cancel_sub?anchor_id="+anchor+"&plan_id="+free+"&reason=1", fan, nil, http.StatusOK)
	if detail(fan, limited)["privilege"] != float64(0) {
		t.Fatal("cancelled subscription retained content access")
	}
	request("POST", "/anchor/api/del_post", outsider, map[string]string{"post_id": approved}, http.StatusForbidden)
	request("POST", "/anchor/api/del_post", owner, map[string]string{"post_id": approved}, http.StatusOK)
	if detail(outsider, approved) != nil {
		t.Fatal("deleted post is still visible")
	}
}

func anchorPostRequest(t *testing.T, platform *app.App, method, route, token, key string, input any, want int) map[string]any {
	t.Helper()
	var body []byte
	if input != nil {
		var err error
		body, err = json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequestWithContext(t.Context(), method, route, bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	platform.Server.Handler.ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("%s %s = %d %s, want %d", method, route, w.Code, w.Body.String(), want)
	}
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("%s response = %q: %v", route, w.Body.String(), err)
	}
	return result
}

func TestPostgresAnchorPostPayment(t *testing.T) {
	config, db := postgresTestDatabase(t)
	var calls, mode atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payment struct {
			User    int64  `json:"user_id"`
			Points  int64  `json:"point"`
			OrderNo string `json:"order_no"`
		}
		if r.Method != http.MethodPost || r.URL.Path != "/api/payment/point/decrease" || r.Header.Get("APPID") != "post-app" ||
			json.NewDecoder(r.Body).Decode(&payment) != nil || r.Header.Get("USER_ID") != strconv.FormatInt(payment.User, 10) ||
			(payment.Points != 99 && payment.Points != 18) || !strings.HasPrefix(payment.OrderNo, "anchor-post-") {
			t.Error("invalid post charge")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		call := calls.Add(1)
		switch mode.Load() {
		case 0:
			fmt.Fprint(w, `{"code":400,"reason":"POINT_NOT_ENOUGH"}`)
		case 1:
			fmt.Fprintf(w, `{"code":200,"data":{"order_id":"%d","order_no":%q,"total_point":%d,"total_bad_debt_point":1}}`,
				call, "provider-"+payment.OrderNo, payment.Points)
		case 2:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer provider.Close()
	config.PaymentHost, config.PaymentAppID = provider.URL, "post-app"
	first, err := app.New(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(first.Close)
	second, err := app.New(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Close)
	owner := subscriptionTestUser(t, first, "paid-post-owner", true)
	fan := subscriptionTestUser(t, first, "paid-post-fan", false)
	other := subscriptionTestUser(t, first, "paid-post-other", false)
	create := func() string {
		t.Helper()
		result := anchorPostRequest(t, first, "POST", "/anchor/api/create_post", owner.Token, "",
			map[string]any{"content": "paid content", "price": 99,
				"url": []map[string]string{{"url": "https://media.invalid/paid.jpg"}}}, http.StatusOK)
		return strconv.FormatInt(int64(result["result"].(map[string]any)["post_id"].(float64)), 10)
	}
	postID := create()
	path := "/api/sub/pay_post?post_id=" + postID
	view := func(actor auth.Session) map[string]any {
		t.Helper()
		return anchorPostRequest(t, second, "GET", "/anchor/api/get_one_post/"+postID, actor.Token, "", nil, http.StatusOK)["data"].(map[string]any)
	}
	pay := func(actor auth.Session, code int) map[string]any {
		t.Helper()
		return anchorPostRequest(t, first, "GET", path, actor.Token, "", nil, code)
	}
	pay(owner, http.StatusForbidden)
	if result := pay(fan, http.StatusOK); result["status"] != "4046" || calls.Load() != 1 || view(fan)["privilege"] != float64(0) {
		t.Fatalf("declined payment = %+v, calls = %d", result, calls.Load())
	}
	mode.Store(1)
	for range 2 {
		if paid := pay(fan, http.StatusOK); paid["status"] != "200" {
			t.Fatalf("purchase = %+v", paid)
		}
	}
	if calls.Load() != 2 || view(fan)["privilege"] != float64(1) || view(other)["privilege"] != float64(0) {
		t.Fatal("confirmed purchase was duplicated or applied to the wrong account")
	}
	query := anchorPostRequest(t, second, "GET", "/anchor/api/query_post?post_id="+postID, owner.Token, "", nil, http.StatusOK)["data"].(map[string]any)
	if query["is_pay"] != true || query["sub_num"] != float64(1) {
		t.Fatalf("owner paid count = %+v", query)
	}
	mode.Store(2)
	pending := pay(other, http.StatusServiceUnavailable)
	mode.Store(1)
	retry := pay(other, http.StatusServiceUnavailable)
	if pending["order_no"] == "" || pending["order_no"] != retry["order_no"] || calls.Load() != 3 || view(other)["privilege"] != float64(0) {
		t.Fatalf("pending = %+v, retry = %+v, calls = %d", pending, retry, calls.Load())
	}
	tip := func(actor, named auth.Session, id, key string, code int) map[string]any {
		t.Helper()
		return anchorPostRequest(t, second, "POST", "/anchor/api/donate_post", actor.Token, key, map[string]any{
			"post_id": id, "user_id": strconv.FormatInt(named.User.ID, 10), "amount": 18,
		}, code)
	}
	tip(other, fan, postID, "tip1", http.StatusForbidden)
	tip(other, other, postID, "tip1", http.StatusForbidden)
	for range 2 {
		if result := tip(fan, fan, postID, "tip1", http.StatusOK); result["status"] != "200" {
			t.Fatalf("tip = %+v", result)
		}
	}
	rank := anchorPostRequest(t, second, "GET", "/anchor/api/get_post_donate_rank?post_id="+postID+"&page=1&page_size=20",
		owner.Token, "", nil, http.StatusOK)["data"].([]any)
	if len(rank) != 1 || rank[0].(map[string]any)["amount"] != float64(18) || view(fan)["my_tip_amount"] != float64(18) ||
		calls.Load() != 4 {
		t.Fatalf("tip was not counted exactly once: rank=%+v calls=%d", rank, calls.Load())
	}
	ownerNotices := subscriptionGet(t, second, owner, "/api/notice/page_notice?page=1&pageSize=20")["data"].(map[string]any)["list"].([]any)
	if len(ownerNotices) != 1 || ownerNotices[0].(map[string]any)["noticeType"] != float64(9) {
		t.Fatalf("confirmed tip notices = %+v", ownerNotices)
	}
	level := subscriptionGet(t, second, fan, "/api/sub/level/"+strconv.FormatInt(owner.User.ID, 10))["data"].(map[string]any)
	if level["platform_level"] != float64(14) || level["fans_level"] != float64(0) {
		t.Fatalf("post debits counted under the wrong experience level: %+v", level)
	}
	var confirmed int
	if err := db.QueryRow(t.Context(), `SELECT count(*) FROM anchor_post_orders WHERE state=1 AND provider_id IS NOT NULL`).Scan(&confirmed); err != nil || confirmed != 2 {
		t.Fatalf("confirmed rows = %d, error = %v", confirmed, err)
	}
	for kind, want := range map[int]float64{3: 99, 4: 18} {
		data := anchorPostRequest(t, second, "GET", fmt.Sprintf("/anchor/api/stat_income_sometime?date_type=1&income_type=%d", kind),
			owner.Token, "", nil, http.StatusOK)["data"].(map[string]any)
		if data["sum"] != want {
			t.Fatalf("earnings for kind %d = %+v", kind, data)
		}
	}
}
