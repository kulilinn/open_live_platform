package app_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"open_live_platform/internal/admin"
)

func TestPostgresPlatformAccessAndWithdrawalSettings(t *testing.T) {
	api := startEditorialAPI(t)
	type object = map[string]any
	const black = "/api/operation/usermanage/black/"
	const white = "/api/operation/platform/whiteuser/"
	const fees = "/api/finance/withdrawalaudit/withdrawalsettings/"
	// A user in the operations tenant must not be able to edit financial settings.
	api.call(t, fees+"get", nil, 403, nil)
	finance := api
	finance.tenant = admin.FinanceTenant
	finance.token = ""
	var login struct{ AccessToken string }
	finance.call(t, "/api/auth/accountlogin", object{"username": "finance", "password": api.password}, 200, &login)
	finance.token = login.AccessToken
	finance.call(t, white+"set", object{"content": "abc"}, 403, nil)
	var limits map[string]string
	finance.call(t, fees+"get", nil, 200, &limits)
	if limits["minWithdrawalAmount"] != "0" {
		t.Fatal(limits)
	}
	input := object{"withdrawal_fee_ratio": "25", "min_withdrawal_fee": "0.05", "min_withdrawal_amount": "1.01"}
	finance.call(t, fees+"set", input, 200, &limits)
	input["min_withdrawal_fee"] = "-0.1"
	finance.call(t, fees+"set", input, 400, nil)
	finance.call(t, fees+"get", nil, 200, &limits)
	if limits["minWithdrawalFee"] != "0.05" {
		t.Fatal(limits)
	}
	user, err := api.platform.Accounts.SignUp(t.Context(), "viewer@example.test", "test-secret", "Viewer")
	if err != nil {
		t.Fatal(err)
	}
	signin := func() string {
		t.Helper()
		w := httptest.NewRecorder()
		api.public.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/auth/signin?compat=rust", strings.NewReader(`{"email":"viewer@example.test","password":"test-secret"}`)))
		var resp struct {
			User struct {
				Tag string `json:"x_user_tag"`
			}
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &resp) != nil {
			t.Fatal(w.Body.String())
		}
		return resp.User.Tag
	}
	api.call(t, white+"set", object{"content": "other-viewer@example.test"}, 200, nil)
	if tag := signin(); tag != "" {
		t.Fatal(tag)
	}
	api.call(t, white+"set", object{"content": "other@example.test,viewer@example.test"}, 200, nil)
	if tag := signin(); tag != "stuff" {
		t.Fatal(tag)
	}
	// httptest's direct peer is 192.0.2.1; it must not match 192.0.2.10.
	api.call(t, black+"setips", object{"content": "192.0.2.10"}, 200, nil)
	get := func() int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/user/platform_info", nil)
		r.Header.Set("Authorization", "Bearer "+user.Token)
		r.Header.Set("X-Forwarded-For", "203.0.113.10")
		api.public.ServeHTTP(w, r)
		return w.Code
	}
	if got := get(); got != 200 {
		t.Fatal(got)
	}
	now := time.Now().UTC()
	period := object{"startTimeToday": now.Add(-time.Hour).Format(time.RFC3339), "endTimeToday": now.Add(time.Hour).Format(time.RFC3339)}
	var stats admin.PlatformStats
	api.call(t, "/api/operation/platform/statistic/index", period, 200, &stats)
	if stats.Users != 1 || stats.NewUsers != 1 || stats.Logins != 1 || stats.Online != 1 {
		t.Fatalf("platform stats: %+v", stats)
	}
	var list struct {
		Total int
		List  []struct{ LastLoginTime *time.Time }
	}
	api.call(t, "/api/operation/usermanage/user/page", object{"loginStartTime": period["startTimeToday"], "loginEndTime": period["endTimeToday"]}, 200, &list)
	if list.Total != 1 || list.List[0].LastLoginTime == nil {
		t.Fatalf("login activity: %+v", list)
	}
	api.call(t, black+"setips", object{"content": "192.0.2.1"}, 200, nil)
	if got := get(); got != 401 {
		t.Fatal(got)
	}
	api.call(t, black+"setips", object{"content": ""}, 200, nil)
	api.call(t, "/api/operation/usermanage/user/account_reset", object{"userId": strconv.FormatInt(user.User.ID, 10), "reason": []string{"profile"}}, 200, nil)
	u, ok, err := api.platform.Accounts.Find(t.Context(), user.User.ID)
	if err != nil || !ok || u.Account != "" || u.Email != user.User.Email {
		t.Fatalf("reset user: %+v, %v", u, err)
	}
	var violations struct {
		Total int
		List  []object
	}
	api.call(t, "/api/operation/usermanage/user/violation_records", object{"id": strconv.FormatInt(u.ID, 10)}, 200, &violations)
	if violations.Total != 1 {
		t.Fatal(violations)
	}
	if got := get(); got != 200 {
		t.Fatal(got)
	}
}
