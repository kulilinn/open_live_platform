package app_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/subscription"
)

func TestFreeSubscriptionPromotionAndBundleManagement(t *testing.T) {
	t.Run("memory", func(t *testing.T) { testSubscriptionOffers(t, app.Config{}) })
	t.Run("postgres", func(t *testing.T) {
		config, _ := postgresTestDatabase(t)
		testSubscriptionOffers(t, config)
	})
}

func testSubscriptionOffers(t *testing.T, config app.Config) {
	t.Helper()
	var charged atomic.Int64
	billing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		charged.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer billing.Close()
	config.PaymentHost, config.PaymentAppID = billing.URL, "trial-test"
	plat, err := app.New(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer plat.Close()
	owner := subscriptionTestUser(t, plat, "trial-host", true)
	stranger := subscriptionTestUser(t, plat, "trial-stranger", true)
	fan := subscriptionTestUser(t, plat, "trial-fan", false)
	late := subscriptionTestUser(t, plat, "trial-late", false)
	post := func(user auth.Session, route string, input map[string]any, status int) map[string]any {
		t.Helper()
		return authFlowRequest(t, plat.Server.Handler, "/api/sub/"+route, user.Token, input, status)
	}
	plan := post(owner, "anchor/create_pay", map[string]any{"price": 990, "name": "Monthly"}, http.StatusOK)["result"].(map[string]any)["plan_id"].(string)
	planID := map[string]any{"plan_id": plan}
	anchorID := strconv.FormatInt(owner.User.ID, 10)
	findPlan := func() map[string]any {
		t.Helper()
		listed := post(fan, "get_anchor_plan", map[string]any{"anchor_id": anchorID}, http.StatusOK)["data"].([]any)
		for _, entry := range listed {
			if val := entry.(map[string]any); val["id"] == plan {
				return val
			}
		}
		t.Fatal("paid plan missing from public list")
		return nil
	}
	bundles := map[string]any{"plan_id": plan, "list": []map[string]any{{"id": "0", "promotion_time": 90, "discount": 0.2, "price": 2400}}}
	if got := post(stranger, "anchor/create_buddle", bundles, http.StatusOK); got["status"] != "4005" {
		t.Fatalf("non-owner bundle save = %+v", got)
	}
	ids := post(owner, "anchor/create_buddle", bundles, http.StatusOK)["result"].(map[string]any)["list"].([]any)
	var saved []subscription.Bundle
	if err := json.Unmarshal([]byte(findPlan()["buddle"].(string)), &saved); err != nil || len(saved) != 1 || strconv.FormatInt(saved[0].ID, 10) != ids[0] {
		t.Fatalf("bundle payload mismatch: %+v, %v", saved, err)
	}
	post(owner, "anchor/del_buddle", map[string]any{"plan_id": plan, "buddle_id": ids[0]}, http.StatusOK)
	if got := findPlan()["buddle"]; got != "[]" {
		t.Fatalf("bundle remained after deletion: %v", got)
	}
	offer := map[string]any{"plan_id": plan, "promotion": map[string]any{"promotion_time": 7, "discount": 0.2, "remark": "Trial"}}
	if got := post(stranger, "anchor/promote", offer, http.StatusOK); got["status"] != "4005" {
		t.Fatalf("non-owner promotion save = %+v", got)
	}
	post(owner, "anchor/promote", offer, http.StatusOK)
	published := findPlan()
	var promotion subscription.Promotion
	if err := json.Unmarshal([]byte(published["promotion"].(string)), &promotion); err != nil || published["is_promotion"] != float64(1) || promotion.Days != 7 {
		t.Fatalf("published promotion = %+v, %v", published, err)
	}
	trial := map[string]any{"anchor_id": anchorID, "plan_id": plan, "promotion": 1}
	if got := post(fan, "pay_sub", trial, http.StatusOK); got["status"] != "200" {
		t.Fatalf("free trial response = %+v", got)
	}
	subscriptionDMAllowed(t, plat, fan, owner, true)
	if got := post(fan, "pay_sub", trial, http.StatusForbidden); got["status"] != "4016" {
		t.Fatalf("repeated trial response = %+v", got)
	}
	rows := post(fan, "get_sub", map[string]any{"anchor_id": anchorID}, http.StatusOK)["data"].([]any)
	item := rows[0].(map[string]any)
	if item["first_sub"] != float64(2) || item["is_promotion_purchased"] != float64(1) {
		t.Fatalf("trial relationship = %+v", item)
	}
	post(owner, "anchor/unpromote", planID, http.StatusOK)
	if got := post(late, "pay_sub", trial, http.StatusOK); got["status"] != "4005" {
		t.Fatalf("withdrawn offer remained available: %+v", got)
	}
	subscriptionDMAllowed(t, plat, fan, owner, true)
	if calls := charged.Load(); calls != 0 {
		t.Fatalf("free trial sent %d payment requests", calls)
	}
}
