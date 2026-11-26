package app_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/payment"
)

func TestPaymentBoundary(t *testing.T) {
	testPaymentBoundary(t, app.Config{}, false)
}

func TestPostgresPaymentBoundary(t *testing.T) {
	config, _ := postgresTestDatabase(t)
	testPaymentBoundary(t, config, true)
}

func testPaymentBoundary(t *testing.T, config app.Config, reopen bool) {
	t.Helper()
	ctx := t.Context()
	var requests, orders, notices atomic.Int64
	var userID atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid := strconv.FormatInt(userID.Load(), 10)
		if r.Header.Get("APPID") != "platform" || r.Header.Get("USER_ID") != uid || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/payment/wallet":
			requests.Add(1)
			if r.Header.Get("BORROW_APPID") != "borrow" || r.URL.RawQuery != "user_id="+uid+"&currency=JPY" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Set-Cookie", "private-provider-cookie=1234")
			fmt.Fprint(w, `{"code":200,"data":{"points":9007199254740993}}`)
		case "POST /api/payment/refund_request":
			requests.Add(1)
			body, err := io.ReadAll(r.Body)
			if err != nil || string(body) != `{"order_id":"9007199254740993","userId":"`+uid+`"}` || r.Header.Get("Idempotency-Key") != "provider-refund-test" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"code":202,"data":{"state":"pending"}}`)
		case "POST /api/payment/large_order/create":
			var body struct {
				UserID  int64  `json:"user_id"`
				Region  string `json:"region"`
				Point   *int64 `json:"point"`
				Expires *int64 `json:"point_expire_days"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.UserID != userID.Load() || body.Region != "JP" ||
				body.Point == nil || *body.Point != 0 || body.Expires == nil || *body.Expires != 0 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			fmt.Fprintf(w, `{"code":200,"data":{"no":"provider-large-%d"}}`, orders.Add(1))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(provider.Close)
	notification := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Type    int
			Content string
		}
		var email struct {
			Template string `json:"template_key"`
			From     struct{ Address string }
			To       []struct {
				Email struct{ Address string } `json:"email_address"`
			}
			Merge map[string]string `json:"merge_info"`
		}
		if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&body) != nil || body.Type != 3 ||
			json.Unmarshal([]byte(body.Content), &email) != nil || email.Template != "jp-template" ||
			email.From.Address != "merchant@example.test" || len(email.To) != 1 || email.To[0].Email.Address != "buyer@example.test" ||
			email.Merge["nick_name"] != "buyer" || email.Merge["search_name"] != "buyer" || email.Merge["email"] != "buyer@example.test" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if notices.Add(1) > 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(notification.Close)
	config.PaymentHost, config.PaymentAppID, config.PaymentBorrowAppID = provider.URL, "platform", "borrow"
	config.PaymentProxyRoutes = "GET /api/payment/wallet, POST /api/payment/refund_request"
	config.LargeOrders = payment.LargeOrderConfig{Region: "JP", NotificationURL: notification.URL,
		FromAddress: "merchant@example.test", FromName: "Merchant", TemplateJP: "jp-template"}
	open := func() *app.App {
		t.Helper()
		platform, err := app.New(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(platform.Close)
		return platform
	}
	platform := open()
	signUp := func(name string) auth.Session {
		t.Helper()
		session, err := platform.Accounts.SignUp(ctx, name+"@example.test", "password", name)
		if err != nil {
			t.Fatal(err)
		}
		return session
	}
	user, other := signUp("buyer"), signUp("other")
	userID.Store(user.User.ID)
	id := strconv.FormatInt(user.User.ID, 10)
	otherID := strconv.FormatInt(other.User.ID, 10)
	const prefix = "/api/payment/"
	request := func(method, endpoint string, session auth.Session, key, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, prefix+endpoint, strings.NewReader(body))
		if session.Token != "" {
			r.Header.Set("Authorization", "Bearer "+session.Token)
		}
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("APPID", "untrusted")
		r.Header.Set("USER_ID", otherID)
		r.Header.Set("BORROW_APPID", "untrusted")
		r.Header.Set("Cookie", "client-cookie=1234")
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		w := httptest.NewRecorder()
		platform.Server.Handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s = %d, want %d: %s", method, endpoint, w.Code, want, w.Body.String())
		}
		return w
	}
	assertChecks := func(who auth.Session, want payment.Checks) {
		t.Helper()
		w := request(http.MethodGet, "check/get", who, "", "", http.StatusOK)
		var result struct {
			Status string
			Data   payment.Checks
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Status != "200" || result.Data != want {
			t.Fatalf("payment checks = %+v, want %+v, err = %v", result, want, err)
		}
	}
	assertChecks(user, payment.Checks{})
	request(http.MethodGet, "check/edit?confirm_check=true", user, "", "", http.StatusBadRequest)
	request(http.MethodGet, "check/edit?confirm_check=true&adult_check=true&confirm_pay_check=true", user, "", "", http.StatusOK)
	wantChecks := payment.Checks{Confirm: true, Adult: true, ConfirmPayment: true}
	assertChecks(user, wantChecks)
	assertChecks(other, payment.Checks{})
	request(http.MethodGet, "wallet", auth.Session{}, "", "", http.StatusUnauthorized)
	request(http.MethodGet, "wallet?user_id="+otherID, user, "", "", http.StatusBadRequest)
	request(http.MethodPost, "point/decrease", user, "", `{"user_id":`+id+`,"point":100}`, http.StatusNotFound)
	request(http.MethodPost, "refund_request", user, "", `{"uid":"`+otherID+`","uid":"`+id+`"}`, http.StatusBadRequest)
	request(http.MethodGet, "%77allet", user, "", "", http.StatusNotFound)
	wallet := request(http.MethodGet, "wallet?user_id="+id+"&currency=JPY", user, "", "", http.StatusOK)
	if wallet.Header().Get("Set-Cookie") != "" || wallet.Body.String() != `{"code":200,"data":{"points":9007199254740993}}` {
		t.Fatalf("wallet result = %s, headers = %v", wallet.Body.String(), wallet.Header())
	}
	refund := request(http.MethodPost, "refund_request", user, "provider-refund-test", `{"order_id":"9007199254740993","userId":"`+id+`"}`, http.StatusAccepted)
	if refund.Body.String() != `{"code":202,"data":{"state":"pending"}}` || requests.Load() != 2 {
		t.Fatalf("provider refund result = %s, calls=%d", refund.Body.String(), requests.Load())
	}
	apply := func(key string, status int) {
		t.Helper()
		w := request(http.MethodPost, "large_order/apply", user, key, "", status)
		if w.Header().Get("Idempotency-Key") != key || (status == http.StatusOK && strings.TrimSpace(w.Body.String()) != `{"orderId":0}`) {
			t.Fatalf("large order %q = %s, headers=%v", key, w.Body.String(), w.Header())
		}
	}
	apply("order-finished", http.StatusOK)
	apply("order-email-indeterminate", http.StatusConflict)
	if reopen {
		platform.Close()
		platform = open()
	}
	assertChecks(user, wantChecks)
	apply("order-finished", http.StatusOK)
	apply("order-email-indeterminate", http.StatusConflict)
	if orders.Load() != 2 || notices.Load() != 2 {
		t.Fatalf("large order replay contacted upstream again: orders=%d notifies=%d", orders.Load(), notices.Load())
	}
}
