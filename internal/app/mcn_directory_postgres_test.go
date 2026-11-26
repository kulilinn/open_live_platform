package app_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"open_live_platform/internal/admin"
	"open_live_platform/internal/app"
)

func TestPostgresMCNDirectory(t *testing.T) {
	config, db := postgresTestDatabase(t)
	ctx := t.Context()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	const password = "fixture-backoffice-pass"
	hash, err := admin.SystemPassword(password)
	check(err)
	accounts, err := json.Marshal([]admin.Principal{
		{ID: 51, Area: admin.OperationArea, Tenant: admin.FinanceTenant, Username: "finance", Admin: true, PasswordHash: hash},
		{ID: 52, Area: admin.OperationArea, Tenant: admin.OperationTenant, Username: "operator", Admin: true, PasswordHash: hash},
		{ID: 53, Area: admin.MCNArea, AgencyID: 53, Email: "existing@example.test", Admin: true, PasswordHash: hash},
	})
	check(err)
	config.AdminAddr, config.MCNAdminAddr = "127.0.0.1:0", "127.0.0.1:0"
	config.MCNInternalToken = "local-test-only-finance-service"
	config.AdminAccountsFile = filepath.Join(t.TempDir(), "accounts.json")
	check(os.WriteFile(config.AdminAccountsFile, accounts, 0600))
	a, err := app.New(ctx, config)
	check(err)
	t.Cleanup(a.Close)
	fin, mcn := a.Backoffice.Handler, a.MCNBackoffice.Handler
	request := func(handler http.Handler, path, token, tenant, service, body string, code int, status string) json.RawMessage {
		t.Helper()
		r := httptest.NewRequestWithContext(ctx, "POST", path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Loverose-Admin", tenant)
		r.Header.Set("X-Inner-Service", service)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		var result struct {
			Status string
			Data   json.RawMessage
		}
		check(json.Unmarshal(w.Body.Bytes(), &result))
		if code != w.Code || status != result.Status {
			t.Fatalf("%s = %d %s, want %d %s", path, w.Code, w.Body.String(), code, status)
		}
		return result.Data
	}
	login := func(h http.Handler, route, tenant, account, bodyKey string, code int, status string) string {
		t.Helper()
		body := fmt.Sprintf(`{%q:%q,"password":%q}`, bodyKey, account, password)
		data := request(h, route, "", tenant, "", body, code, status)
		if code != 200 {
			return ""
		}
		var tokens struct{ AccessToken string }
		check(json.Unmarshal(data, &tokens))
		return tokens.AccessToken
	}
	finance := login(fin, "/api/auth/accountlogin", admin.FinanceTenant, "finance", "username", 200, "200")
	operator := login(fin, "/api/auth/accountlogin", admin.OperationTenant, "operator", "username", 200, "200")
	request(fin, "/api/finance/agency/page", operator, admin.OperationTenant, "", `{}`, 403, "403")
	request(mcn, "/api/agency/organize/page", finance, "", "admin", `{}`, 401, "401")
	request(mcn, "/api/agency/organize/page", config.MCNInternalToken, "", "mcn", `{}`, 401, "401")
	request(mcn, "/api/agency/organize/page", config.MCNInternalToken, "", "admin", `{}`, 200, "200")
	mutate := func(path, body string, code int, status string) json.RawMessage {
		t.Helper()
		return request(fin, "/api/finance"+path, finance, admin.FinanceTenant, "", body, code, status)
	}
	mutate("/template/add", `{"mcnId":"0","name":"Standard","data":[[0,1000,15],[1000,5000,20]]}`, 400, "400")
	mutate("/template/add", `{"mcnId":"0","name":"Standard","data":[[1001,5000,20],[0,1000,15]]}`, 200, "200")
	var templates []admin.MCNTemplate
	check(json.Unmarshal(mutate("/template/page", "", 200, "200"), &templates))
	if len(templates) != 1 || templates[0].Name != "Standard" {
		t.Fatalf("template directory: %+v", templates)
	}
	template := templates[0].ID
	add := func(name, email string, code int, status string) {
		t.Helper()
		mutate("/agency/add", fmt.Sprintf(`{"name":%q,"email":%q,"superName":"Fixture Lead","password":%q,"agencyLevel":1,"sharingTemplateId":"%d"}`,
			name, email, password, template), code, status)
	}
	add("Registry Agency", "new-mcn@example.test", 200, "200")
	add("Duplicate account", "new-mcn@example.test", 409, "409")
	add("Duplicate bootstrap", "existing@example.test", 409, "409")
	var page struct {
		List  []admin.MCNAgencyRow
		Total int64
	}
	check(json.Unmarshal(mutate("/agency/page", `{}`, 200, "200"), &page))
	if page.Total != 1 || len(page.List) != 1 || page.List[0].Staff != 1 || page.List[0].TemplateName == nil || *page.List[0].TemplateName != "Standard" {
		t.Fatalf("agency create, bootstrap, rollback: %+v", page)
	}
	agency := page.List[0].ID
	mcnToken := login(mcn, "/api/user/account_login", "", "new-mcn@example.test", "email", 200, "200")
	home := request(mcn, "/api/home/agency_data", mcnToken, "", "", "", 200, "200")
	if !strings.Contains(string(home), `"name":"Registry Agency"`) || !strings.Contains(string(home), `"adminNum":1`) {
		t.Fatalf("new staff missing its agency scope: %s", home)
	}
	_, err = db.Exec(ctx, `INSERT INTO legacy_business_history(source_table,source_id,owner_id,record) VALUES
		('mcn_settle_platform_statistic','710',$1,'{"ld":9007199254741011,"ratio":25.25,"create_time":"2026-01"}'),
		('mcn_settle_statistic','711',$1,'{"streamer_id":99,"ld":9007199254741011,"ratio":0.25,"create_time":"2026-01-21","live_start_time":1768968000,"live_end_time":1768971600}')`, agency)
	check(err)
	if output := mutate("/income/agency_page", `{"month":"2026-01","mcnName":"registry"}`, 200, "200"); !strings.Contains(string(output), `"ld":9007199254741011,"ratio":"25.25"`) {
		t.Fatalf("finance missed archived monthly records: %s", output)
	}
	if output := mutate("/income/streamer_page", `{"startDate":"2026-01-20","endDate":"2026-01-21","mcnName":"registry"}`, 200, "200"); !strings.Contains(string(output), `"ldTotal":9007199254741011`) {
		t.Fatalf("finance missed archived streamers: %s", output)
	}
	refresh := fmt.Sprintf(`{"mcnId":"%d","startTime":"2026-01-20","endTime":"2026-01-21"}`, agency)
	mutate("/income/refresh_settle_data", refresh, 200, "200")
	request(mcn, "/api/statistic/streamer/finance_refresh_settle_ld", mcnToken, "", "admin", refresh, 401, "401")
	request(mcn, "/api/statistic/streamer/finance_refresh_settle_ld", config.MCNInternalToken, "", "admin", refresh, 200, "200")
	mutate("/agency/edit", fmt.Sprintf(`{"id":"%d","name":"Renamed Agency","sharingTemplateId":"%d"}`, agency, template), 200, "200")
	mutate("/template/edit", fmt.Sprintf(`{"id":"%d","mcnId":"0","name":"Renamed Template","data":[[0,5000,18]]}`, template), 200, "200")
	tiers := mutate("/template/detail", fmt.Sprintf(`{"mcnId":"0","templateId":"%d"}`, template), 200, "200")
	if !strings.Contains(string(tiers), `"ratio":"18"`) || strings.Contains(string(tiers), `"ratio":"15"`) {
		t.Fatalf("template edit did not replace tiers in place: %s", tiers)
	}
	deleteTemplate := fmt.Sprintf(`{"id":"%d"}`, template)
	mutate("/template/delete", deleteTemplate, 409, "409")
	mutate("/agency/delete", fmt.Sprintf(`{"id":"%d"}`, agency), 200, "200")
	request(mcn, "/api/home/agency_data", mcnToken, "", "", "", 401, "401")
	login(mcn, "/api/user/account_login", "", "new-mcn@example.test", "email", 401, "401")
	mutate("/template/delete", deleteTemplate, 200, "200")
	if data := mutate("/template/page", "", 200, "200"); string(data) != "[]" {
		t.Fatalf("deleted template still in directory: %s", data)
	}
	var changes int64
	check(db.QueryRow(ctx, `SELECT count(*) FROM backoffice_logs WHERE operator_id=51 AND tenant='finance-admin' AND action IN
		('mcn_agency_add','mcn_agency_edit','mcn_agency_delete','mcn_template_save','mcn_template_delete')`).Scan(&changes))
	if changes != 6 {
		t.Fatalf("successful finance mutations logged %d times", changes)
	}
}
