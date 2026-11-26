package app_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/dm"
)

func TestSubscriptionPlansAndFreeDM(t *testing.T) {
	t.Run("memory", func(t *testing.T) { testSubscriptionPlansAndFreeDM(t, app.Config{}) })
	t.Run("postgres", func(t *testing.T) {
		config, _ := postgresTestDatabase(t)
		testSubscriptionPlansAndFreeDM(t, config)
	})
}

func testSubscriptionPlansAndFreeDM(t *testing.T, config app.Config) {
	t.Helper()
	platform, err := app.New(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	anchor := subscriptionTestUser(t, platform, "anchor", true)
	otherAnchor := subscriptionTestUser(t, platform, "other-anchor", true)
	fan := subscriptionTestUser(t, platform, "fan", false)

	post := func(account auth.Session, route string, fields map[string]any, want int) map[string]any {
		t.Helper()
		return authFlowRequest(t, platform.Server.Handler, "/api/sub/"+route, account.Token, fields, want)
	}
	planID := func(result map[string]any) string {
		t.Helper()
		return result["result"].(map[string]any)["plan_id"].(string)
	}
	const paidPrice = 990
	post(fan, "anchor/create_pay", map[string]any{"name": "Paid", "price": paidPrice}, http.StatusForbidden)
	paid := planID(post(anchor, "anchor/create_pay", map[string]any{"name": "Monthly", "price": paidPrice}, http.StatusOK))
	if result := post(otherAnchor, "anchor/unpub", map[string]any{"plan_id": paid}, http.StatusOK); result["status"] != "4005" {
		t.Fatalf("non-owner unpublished plan: %+v", result)
	}
	post(anchor, "anchor/unpub", map[string]any{"plan_id": paid}, http.StatusOK)
	anchorID := strconv.FormatInt(anchor.User.ID, 10)
	planList := func(actor auth.Session, target string) []any {
		return post(actor, "get_anchor_plan", map[string]any{"anchor_id": target}, http.StatusOK)["data"].([]any)
	}
	visible := planList(fan, anchorID)
	if len(visible) != 1 || visible[0].(map[string]any)["ty"] != float64(2) {
		t.Fatalf("public plans = %+v, want only free", visible)
	}
	free := visible[0].(map[string]any)["id"].(string)
	if own := planList(anchor, "0"); len(own) != 2 {
		t.Fatalf("owner plans = %+v, want hidden plan too", own)
	}
	post(anchor, "anchor/edit", map[string]any{"plan_id": paid, "name": "Updated", "price": 1200}, http.StatusOK)
	post(anchor, "anchor/pub", map[string]any{"plan_id": paid}, http.StatusOK)
	if len(planList(fan, anchorID)) != 2 {
		t.Fatal("owner publication did not expose paid plan")
	}
	post(fan, "pay_sub", map[string]any{"anchor_id": anchorID, "plan_id": paid}, http.StatusServiceUnavailable)
	if _, _, err := platform.DM.FindDMConversation(t.Context(), fan.User.ID, anchor.User.ID); !errors.Is(err, dm.ErrNotFound) {
		t.Fatalf("failed payment opened a DM: %v", err)
	}
	post(anchor, "anchor/set_free_sub", map[string]any{"open": 0}, http.StatusOK)
	sub := map[string]any{"anchor_id": anchorID, "plan_id": "0"}
	if result := post(fan, "free_sub", sub, http.StatusOK); result["status"] != "4005" {
		t.Fatalf("unpublished free plan accepted: %+v", result)
	}
	post(anchor, "anchor/set_free_sub", map[string]any{"open": 1}, http.StatusOK)
	for range 2 {
		if result := post(fan, "free_sub", sub, http.StatusOK); result["status"] != "200" {
			t.Fatalf("free subscribe = %+v", result)
		}
	}
	id := subscriptionDMAllowed(t, platform, fan, anchor, true)
	rows := post(fan, "get_sub", sub, http.StatusOK)["data"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["plan_id"] != free {
		t.Fatalf("free subscriptions after duplicate = %+v", rows)
	}
	anchorRows := post(anchor, "get_my_sub", map[string]any{"andience": strconv.FormatInt(fan.User.ID, 10)}, http.StatusOK)["data"].([]any)
	if len(anchorRows) != 1 {
		t.Fatalf("anchor relationship list = %+v", anchorRows)
	}
	if got := subscriptionGet(t, platform, fan, "/api/sub/fans/subscription_list?page=1&page_size=20")["data"].([]any); len(got) != 1 {
		t.Fatalf("fan subscription list = %+v", got)
	}
	cancelPath := "/api/sub/cancel_sub?anchor_id=" + anchorID + "&plan_id=" + free + "&reason=1"
	if result := subscriptionGet(t, platform, otherAnchor, cancelPath); result["status"] != "4005" {
		t.Fatalf("stranger cancelled relationship: %+v", result)
	}
	subscriptionDMAllowed(t, platform, fan, anchor, true)
	if result := subscriptionGet(t, platform, fan, cancelPath); result["status"] != "200" {
		t.Fatalf("cancel subscription = %+v", result)
	}
	subscriptionDMAllowed(t, platform, fan, anchor, false)
	post(fan, "free_sub", sub, http.StatusOK)
	if reopened := subscriptionDMAllowed(t, platform, fan, anchor, true); reopened != id {
		t.Fatalf("resubscription DM ID = %d, want %d", reopened, id)
	}
	page, err := platform.DM.DMMessages(t.Context(), fan.User.ID, id, dm.NewPageRequest(1, 20))
	if err != nil || len(page.Data) != 1 || page.Data[0].Content != dm.Welcome {
		t.Fatalf("idempotent welcome count = %d, error = %v", len(page.Data), err)
	}
}

func TestPostgresPaidSubscriptionVerifiesProviderBeforeGrant(t *testing.T) {
	config, db := postgresTestDatabase(t)
	var debitCalls, providerMode atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			UserID  int64  `json:"user_id"`
			Point   int64  `json:"point"`
			OrderNo string `json:"order_no"`
		}
		if r.Method != http.MethodPost || r.URL.Path != "/api/payment/point/decrease" || r.Header.Get("APPID") != "test-project" ||
			json.NewDecoder(r.Body).Decode(&request) != nil || r.Header.Get("USER_ID") != strconv.FormatInt(request.UserID, 10) ||
			request.Point != 99 || !strings.HasPrefix(request.OrderNo, "sub-") {
			t.Error("invalid payment provider request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		call := debitCalls.Add(1)
		switch providerMode.Load() {
		case 0:
			json.NewEncoder(w).Encode(map[string]any{"code": 400, "reason": "POINT_NOT_ENOUGH"})
		case 1:
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": map[string]any{
				"order_id": strconv.FormatInt(call, 10), "order_no": "provider-" + request.OrderNo, "total_point": request.Point,
			}})
		case 2:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer provider.Close()
	config.PaymentHost, config.PaymentAppID = provider.URL, "test-project"
	platform, err := app.New(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	anchor := subscriptionTestUser(t, platform, "paid-anchor", true)
	fan := subscriptionTestUser(t, platform, "paid-fan", false)
	unknown := subscriptionTestUser(t, platform, "unknown-payment", false)
	anchorID := strconv.FormatInt(anchor.User.ID, 10)
	result := authFlowRequest(t, platform.Server.Handler, "/api/sub/anchor/create_pay", anchor.Token,
		map[string]any{"price": 990, "name": "Monthly"}, http.StatusOK)
	plan := result["result"].(map[string]any)["plan_id"].(string)
	pay := func(viewer auth.Session, status int) map[string]any {
		t.Helper()
		return authFlowRequest(t, platform.Server.Handler, "/api/sub/pay_sub", viewer.Token, map[string]any{"anchor_id": anchorID, "plan_id": plan}, status)
	}
	wantNoDM := func(viewer auth.Session) {
		t.Helper()
		if _, _, err := platform.DM.FindDMConversation(t.Context(), viewer.User.ID, anchor.User.ID); !errors.Is(err, dm.ErrNotFound) {
			t.Fatalf("unverified payment opened a DM: %v", err)
		}
	}
	if result := pay(fan, http.StatusOK); result["status"] != "4046" {
		t.Fatalf("provider decline = %+v", result)
	}
	wantNoDM(fan)
	providerMode.Store(1)
	if result := pay(fan, http.StatusOK); result["status"] != "200" {
		t.Fatalf("paid subscription response = %+v", result)
	}
	pay(fan, http.StatusOK)
	if got := debitCalls.Load(); got != 2 {
		t.Fatalf("provider calls = %d, want decline and one success", got)
	}
	subscriptionDMAllowed(t, platform, fan, anchor, true)
	if _, err := db.Exec(t.Context(), `WITH expired AS (UPDATE subscription_members SET expires_at=now() WHERE fan_id=$1 RETURNING fan_id)
		UPDATE dm_conversations SET subscription_expires_at=now() WHERE fan_id IN (SELECT fan_id FROM expired)`, fan.User.ID); err != nil {
		t.Fatal(err)
	}
	subscriptionDMAllowed(t, platform, fan, anchor, false)
	providerMode.Store(2)
	first := pay(unknown, http.StatusServiceUnavailable)
	second := pay(unknown, http.StatusServiceUnavailable)
	if first["order_no"] == nil || first["order_no"] != second["order_no"] || debitCalls.Load() != 3 {
		t.Fatalf("uncertain charge was not durably deduplicated: %+v, %+v", first, second)
	}
	wantNoDM(unknown)
}

func subscriptionTestUser(t *testing.T, platform *app.App, name string, anchor bool) auth.Session {
	t.Helper()
	user, err := platform.Accounts.SignUp(t.Context(), name+"@example.org", "password", name)
	if err != nil {
		t.Fatal(err)
	}
	if anchor {
		if _, err := platform.Rooms.Start(t.Context(), user.User.ID, "Live", ""); err != nil {
			t.Fatal(err)
		}
	}
	return user
}

func subscriptionDMAllowed(t *testing.T, platform *app.App, fan, anchor auth.Session, allowed bool) int64 {
	t.Helper()
	id, permission, err := platform.DM.FindDMConversation(t.Context(), fan.User.ID, anchor.User.ID)
	if err != nil || permission != allowed || id < 1 {
		t.Fatalf("DM permission = %t, ID = %d, error = %v, want %t", permission, id, err, allowed)
	}
	return id
}

func subscriptionGet(t *testing.T, platform *app.App, account auth.Session, route string) map[string]any {
	t.Helper()
	if _, err := url.ParseRequestURI(route); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, route, nil)
	req.Header.Set("Authorization", "Bearer "+account.Token)
	w := httptest.NewRecorder()
	platform.Server.Handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("%s = HTTP %d: %s", route, w.Code, w.Body.String())
	}
	var response map[string]any
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	return response
}
