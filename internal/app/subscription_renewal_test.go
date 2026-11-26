package app_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
)

func TestPostgresSubscriptionRenewalRequiresVerifiedPriorPayment(t *testing.T) {
	config, db := postgresTestDatabase(t)
	var chargeCalls, providerMode atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			No    string `json:"order_no"`
			Point int64  `json:"point"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.No == "" || input.Point != 99 {
			t.Errorf("renewal debit has invalid fields: %+v, %v", input, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		call := chargeCalls.Add(1)
		switch providerMode.Load() {
		case 0:
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": map[string]any{
				"order_id": strconv.FormatInt(call, 10), "order_no": "provider-" + input.No, "total_point": input.Point,
			}})
		case 1:
			json.NewEncoder(w).Encode(map[string]any{"code": 400, "reason": "POINT_NOT_ENOUGH"})
		case 2:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer provider.Close()
	config.PaymentHost, config.PaymentAppID = provider.URL, "renewal-test"
	config.SubscriptionRenewToken = "internal-scheduler-test-token"
	start := func() *app.App {
		t.Helper()
		platform, err := app.New(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(platform.Close)
		return platform
	}
	plat, second := start(), start()
	anchor := subscriptionTestUser(t, plat, "renewal-anchor", true)
	fan := subscriptionTestUser(t, plat, "renewal-fan", false)
	trial := subscriptionTestUser(t, plat, "renewal-trial", false)
	declined := subscriptionTestUser(t, plat, "renewal-declined", false)
	pending := subscriptionTestUser(t, plat, "renewal-pending", false)
	post := func(actor auth.Session, route string, input map[string]any) map[string]any {
		t.Helper()
		return authFlowRequest(t, plat.Server.Handler, "/api/sub/"+route, actor.Token, input, http.StatusOK)
	}
	plan := post(anchor, "anchor/create_pay", map[string]any{"price": 990, "name": "Monthly"})["result"].(map[string]any)["plan_id"].(string)
	anchorID := strconv.FormatInt(anchor.User.ID, 10)
	order := map[string]any{"plan_id": plan, "anchor_id": anchorID}
	post(anchor, "anchor/promote", map[string]any{"plan_id": plan, "promotion": map[string]any{"promotion_time": 1, "discount": 0.1}})
	post(trial, "pay_sub", map[string]any{"plan_id": plan, "anchor_id": anchorID, "promotion": 1})
	for _, buyer := range []auth.Session{fan, declined, pending} {
		if response := post(buyer, "pay_sub", order); response["status"] != "200" {
			t.Fatalf("initial payment = %+v", response)
		}
	}
	now := time.Now().UTC().Truncate(time.Second)
	nearExpiry := now.Add(time.Hour)
	moveExpiry := func(buyer auth.Session) {
		t.Helper()
		_, err := db.Exec(t.Context(), `UPDATE subscription_members SET expires_at=$2 WHERE fan_id=$1`, buyer.User.ID, nearExpiry)
		if err != nil {
			t.Fatal(err)
		}
	}
	get := func(buyer auth.Session) map[string]any {
		t.Helper()
		return post(buyer, "get_sub", order)["data"].([]any)[0].(map[string]any)
	}
	run := func(target *app.App, token string, httpCode int) map[string]any {
		t.Helper()
		return authFlowRequest(t, target.Server.Handler, "/api/sub/renew_sub", token, nil, httpCode)
	}
	moveExpiry(fan)
	moveExpiry(trial)
	run(plat, anchor.Token, http.StatusUnauthorized)
	post(anchor, "anchor/edit", map[string]any{"plan_id": plan, "price": 1200, "name": "Monthly"})
	run(plat, config.SubscriptionRenewToken, http.StatusOK)
	if count := chargeCalls.Load(); count != 3 {
		t.Fatalf("price change triggered a renewal charge, calls = %d", count)
	}
	post(anchor, "anchor/edit", map[string]any{"plan_id": plan, "price": 990, "name": "Monthly"})
	if result := run(second, config.SubscriptionRenewToken, http.StatusOK)["result"].(map[string]any); result["renewed"] != float64(1) {
		t.Fatalf("renewal result = %+v", result)
	}
	if count := chargeCalls.Load(); count != 4 {
		t.Fatalf("expected three initial payments and one renewal, got %d calls", count)
	}
	newExpiry, err := time.Parse(time.RFC3339Nano, get(fan)["expire_at"].(string))
	if err != nil || !newExpiry.After(nearExpiry.AddDate(0, 0, 27)) {
		t.Fatalf("receipt-backed renewal did not extend access: %s, %v", newExpiry, err)
	}
	if got, err := time.Parse(time.RFC3339Nano, get(trial)["expire_at"].(string)); err != nil || !got.Equal(nearExpiry) {
		t.Fatalf("unpaid trial was extended: %s, %v", got, err)
	}
	run(plat, config.SubscriptionRenewToken, http.StatusOK)
	if count := chargeCalls.Load(); count != 4 {
		t.Fatalf("repeat renewal made another charge, calls = %d", count)
	}
	moveExpiry(declined)
	providerMode.Store(1)
	if result := run(plat, config.SubscriptionRenewToken, http.StatusOK)["result"].(map[string]any); result["declined"] != float64(1) || get(declined)["reason"] != float64(7) {
		t.Fatalf("definite renewal decline = %+v, relationship = %+v", result, get(declined))
	}
	subscriptionDMAllowed(t, plat, declined, anchor, false)
	moveExpiry(pending)
	providerMode.Store(2)
	result := run(second, config.SubscriptionRenewToken, http.StatusServiceUnavailable)["result"].(map[string]any)
	pendingNo, _ := result["pending_order"].(string)
	if pendingNo == "" || get(pending)["cancel_at"] != nil {
		t.Fatalf("ambiguous renewal did not preserve pending state: %+v, subscription %+v", result, get(pending))
	}
	req := httptest.NewRequest(http.MethodGet, "/api/sub/cancel_sub?anchor_id="+anchorID+"&plan_id="+plan+"&reason=1", nil)
	req.Header.Set("Authorization", "Bearer "+pending.Token)
	w := httptest.NewRecorder()
	plat.Server.Handler.ServeHTTP(w, req)
	var cancelled map[string]any
	if err := json.NewDecoder(w.Body).Decode(&cancelled); err != nil || w.Code != http.StatusServiceUnavailable || cancelled["order_no"] != pendingNo {
		t.Fatalf("cancel during an unconfirmed renewal returned %d with %+v, %v", w.Code, cancelled, err)
	}
	run(plat, config.SubscriptionRenewToken, http.StatusOK)
	if count := chargeCalls.Load(); count != 6 {
		t.Fatalf("ambiguous renewal caused another debit, calls = %d", count)
	}
}
