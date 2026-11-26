package app_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"open_live_platform/internal/anchor"
	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
)

func TestPostgresAnchorTasksUseVerifiedEvents(t *testing.T) {
	config, db := postgresTestDatabase(t)
	config.AnchorTaskToken = "test-task-service"
	var calls atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var debit struct {
			User   int64  `json:"user_id"`
			Points int64  `json:"point"`
			Order  string `json:"order_no"`
		}
		if r.Method != http.MethodPost || r.URL.Path != "/api/payment/point/decrease" || r.Header.Get("APPID") != "tasks-app" ||
			json.NewDecoder(r.Body).Decode(&debit) != nil || r.Header.Get("USER_ID") != strconv.FormatInt(debit.User, 10) ||
			debit.Order == "" || debit.Points < 1 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if debit.Points == 201 {
			fmt.Fprint(w, `{"code":400,"reason":"POINT_NOT_ENOUGH"}`)
			return
		}
		n := calls.Add(1)
		fmt.Fprintf(w, `{"code":200,"data":{"order_id":"%d","order_no":%q,"total_point":%d}}`, n, "provider-"+debit.Order, debit.Points)
	}))
	t.Cleanup(provider.Close)
	config.PaymentHost, config.PaymentAppID = provider.URL, "tasks-app"
	config.GiftCatalogFile = filepath.Join(t.TempDir(), "tasks-gifts.json")
	if err := os.WriteFile(config.GiftCatalogFile, []byte(`[
		{"id":"1","name":"Flower","price":11},{"id":"2","name":"Rocket","price":200,"effectControlType":6,"parameterOne":"30"},
		{"id":"3","name":"Declined","price":201},{"id":"4","name":"Free","price":0}
	]`), 0600); err != nil {
		t.Fatal(err)
	}
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
	host := subscriptionTestUser(t, first, "task-owner", true)
	ctx := t.Context()
	hostID := strconv.FormatInt(host.User.ID, 10)
	room, err := first.Rooms.Info(ctx, host.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	progress := func(who auth.Session) anchor.TaskProgress {
		t.Helper()
		result := anchorPostRequest(t, second, "GET", "/anchor/api/query_task_progress", who.Token, "", nil, http.StatusOK)["data"]
		raw, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		var state anchor.TaskProgress
		if err := json.Unmarshal(raw, &state); err != nil {
			t.Fatal(err)
		}
		return state
	}
	reconcile := func(who auth.Session, token string, status int) {
		t.Helper()
		path := "/anchor/api/test_task?task_target=3&complete_target_quantity=100000000&user_id=" + hostID
		r := httptest.NewRequestWithContext(ctx, "GET", path, nil)
		r.Header.Set("Authorization", "Bearer "+who.Token)
		r.Header.Set("X-Anchor-Task-Token", token)
		w := httptest.NewRecorder()
		first.Server.Handler.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("test_task = %d %s, want %d", w.Code, w.Body.String(), status)
		}
	}
	reconcile(host, "", http.StatusForbidden)
	reconcile(host, config.AnchorTaskToken, http.StatusOK)
	if state := progress(host); state.Gold != 0 || state.Done != 0 || state.Length != 6 {
		t.Fatalf("untrusted task data affected progress: %+v", state)
	}
	fans := make([]auth.Session, 10)
	free := map[string]string{"anchor_id": hostID, "plan_id": "0"}
	for i := range fans {
		fans[i] = subscriptionTestUser(t, first, fmt.Sprintf("task-fan-%d", i), false)
		anchorPostRequest(t, first, "POST", "/api/sub/free_sub", fans[i].Token, "", free, http.StatusOK)
	}
	for range 3 {
		anchorPostRequest(t, second, "POST", "/api/sub/free_sub", fans[0].Token, "", free, http.StatusOK)
	}
	if state := progress(host); state.Gold != 158 || state.Tasks[0].Progress != 10 {
		t.Fatalf("new followers or reward were counted repeatedly: %+v", state)
	}
	plan := anchorPostRequest(t, first, "POST", "/api/sub/anchor/create_pay", host.Token, "",
		map[string]any{"price": 990, "name": "Task plan"}, http.StatusOK)["result"].(map[string]any)["plan_id"]
	for i := range 3 {
		buy := map[string]any{"anchor_id": hostID, "plan_id": plan}
		anchorPostRequest(t, first, "POST", "/api/sub/pay_sub", fans[i].Token, "", buy, http.StatusOK)
		anchorPostRequest(t, second, "POST", "/api/sub/pay_sub", fans[i].Token, "", buy, http.StatusOK)
	}
	if state := progress(host); state.Gold != 948 || state.Tasks[1].Progress != 3 {
		t.Fatalf("verified paid fan tasks = %+v", state)
	}
	send := func(who auth.Session, id, key string, want string) {
		t.Helper()
		if err := first.Rooms.Join(ctx, room.ID, who.User.ID, ""); err != nil {
			t.Fatal(err)
		}
		body := map[string]any{"date_room_id": hostID, "live_id": strconv.FormatInt(room.Session.LiveID, 10),
			"gift_id": id, "limit_id": "0", "sender_id": strconv.FormatInt(who.User.ID, 10), "receiver_id": hostID,
			"quantity": 1, "unit_price": 1, "sender_role": 5}
		result := anchorPostRequest(t, first, "POST", "/api/date/1vN/give/gifts", who.Token, key, body, http.StatusOK)
		if result["status"] != want {
			t.Fatalf("send gift %s = %+v", id, result)
		}
	}
	send(fans[0], "2", "rocket", "200")
	send(fans[0], "2", "rocket", "200")
	send(fans[0], "1", "same-sender", "200")
	send(fans[1], "1", "second-sender", "200")
	send(fans[2], "3", "declined", "4046")
	send(fans[2], "4", "nonpayment", "200")
	if state := progress(host); state.Gold != 2528 || state.Tasks[2].Progress != 2 || state.Tasks[3].Progress != 1 {
		t.Fatalf("gift tasks included a bad source: %+v", state)
	}
	send(fans[2], "1", "third-sender", "200")
	if _, err := db.Exec(ctx, `UPDATE rooms SET live_started_at=now()-interval '130 minutes' WHERE id=$1`, room.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE room_broadcast_metrics SET started_at=now()-interval '130 minutes' WHERE live_id=$1`, room.Session.LiveID); err != nil {
		t.Fatal(err)
	}
	if state := progress(host); state.Gold != 2686 || state.Tasks[4].Progress != 0 {
		t.Fatalf("unclosed session counted toward a task: %+v", state)
	}
	for range 2 {
		if err := first.Rooms.Close(ctx, room.ID, host.User.ID); err != nil {
			t.Fatal(err)
		}
	}
	reconcile(host, config.AnchorTaskToken, http.StatusOK)
	if state := progress(host); state.Gold != 3476 || state.Done != 5 || state.Tasks[4].Progress != 120 || state.Tasks[5].Progress != 130 {
		t.Fatalf("closed broadcast progress = %+v", state)
	}
	if state := progress(fans[0]); state.Gold != 0 || state.Done != 0 {
		t.Fatalf("another account saw host tasks: %+v", state)
	}
	reconcile(fans[0], config.AnchorTaskToken, http.StatusForbidden)
	var awards int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM anchor_task_rewards WHERE anchor_id=$1`, host.User.ID).Scan(&awards); err != nil || awards != 5 {
		t.Fatalf("award records = %d, err=%v", awards, err)
	}
}

func TestPostgresAnchorTaskExpiryAndDayBoundary(t *testing.T) {
	config, db := postgresTestDatabase(t)
	platform, err := app.New(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(platform.Close)
	host := subscriptionTestUser(t, platform, "task-expiry-owner", true)
	if got := anchor.TaskDay(time.Date(2026, 11, 26, 14, 59, 59, 0, time.UTC)); got != "2026-11-26" {
		t.Fatalf("last second before JST midnight = %s", got)
	}
	if got := anchor.TaskDay(time.Date(2026, 11, 26, 15, 0, 0, 0, time.UTC)); got != "2026-11-27" {
		t.Fatalf("JST midnight = %s", got)
	}
	ctx := t.Context()
	if _, err := db.Exec(ctx, `UPDATE users SET profile='{"IsAnchor":true}' WHERE id=$1`, host.User.ID); err != nil {
		t.Fatal(err)
	}
	observed := time.Now().UTC()
	if _, err := db.Exec(ctx, `INSERT INTO legacy_anchor_star_balances(anchor_id,last_source_id,stars,started_at,observed_at)
		VALUES($1,79591199061446658,120,$2,$3)`, host.User.ID, observed.Add(-anchor.TaskEligibility), observed); err != nil {
		t.Fatal(err)
	}
	status := anchorPostRequest(t, platform, "POST", "/anchor/api/is_anchor", host.Token, "", nil, http.StatusOK)["result"].(map[string]any)
	if status["task_time_expired"] != true {
		t.Fatalf("imported star account expiry = %+v", status)
	}
	room, err := platform.Rooms.Info(ctx, host.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE rooms SET live_started_at=now()-interval '250 minutes' WHERE id=$1`, room.ID); err != nil {
		t.Fatal(err)
	}
	if err := platform.Rooms.Close(ctx, room.ID, host.User.ID); err != nil {
		t.Fatal(err)
	}
	state := anchorPostRequest(t, platform, "GET", "/anchor/api/query_task_progress", host.Token, "", nil, http.StatusOK)["data"].(map[string]any)
	if state["taskGold"] != float64(120) || state["taskOk"] != float64(2) {
		t.Fatalf("expired account was awarded new gold or lost its balance: %+v", state)
	}
}
