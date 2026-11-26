package app_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"open_live_platform/internal/admin"
	"open_live_platform/internal/store/postgres"
)

func TestPostgresBackofficeSystemAcrossReplicas(t *testing.T) {
	config, _ := postgresTestDatabase(t)
	const password = "test-bootstrap-credential"
	hash, err := admin.SystemPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	principals := admin.Principals{
		{ID: 1001, Area: admin.OperationArea, Tenant: admin.OperationTenant, Username: "bootstrap", PasswordHash: hash, Admin: true},
		{ID: 1002, Area: admin.OperationArea, Tenant: admin.FinanceTenant, Username: "bootstrap", PasswordHash: hash, Admin: true},
		{ID: 2001, Area: admin.MCNArea, AgencyID: 9, Email: "admin-one@example.test", PasswordHash: hash, Admin: true},
		{ID: 2002, Area: admin.MCNArea, AgencyID: 10, Email: "admin-two@example.test", PasswordHash: hash, Admin: true},
	}
	file := filepath.Join(t.TempDir(), "backoffice.json")
	content, err := json.Marshal(principals)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, content, 0600); err != nil {
		t.Fatal(err)
	}
	principals, err = admin.ReadPrincipals(file)
	if err != nil {
		t.Fatal(err)
	}
	twoReplicas := func() ([2]string, [2]string) {
		t.Helper()
		var operation, mcn [2]string
		for i := range operation {
			store, err := postgres.Open(t.Context(), config.DatabaseURL)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(store.Shutdown)
			for _, area := range []string{admin.OperationArea, admin.MCNArea} {
				handler, err := admin.BackofficeHandler(area, principals, store, nil)
				if err != nil {
					t.Fatal(err)
				}
				server := httptest.NewServer(handler)
				t.Cleanup(server.Close)
				if area == admin.OperationArea {
					operation[i] = server.URL
				} else {
					mcn[i] = server.URL
				}
			}
		}
		return operation, mcn
	}
	operation, mcn := twoReplicas()
	type reply struct {
		Status string
		Data   json.RawMessage
	}
	request := func(server, path, token, tenant, body string, status int) reply {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Loverose-Admin", tenant)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var v reply
		if r.StatusCode != status || json.Unmarshal(data, &v) != nil {
			t.Fatalf("%s got %d %s, want %d", path, r.StatusCode, data, status)
		}
		return v
	}
	login := func(server, area, username, tenant, password string, status int) string {
		t.Helper()
		path, field := "/api/auth/accountlogin", "username"
		if area == admin.MCNArea {
			path, field = "/api/user/account_login", "email"
		}
		data := request(server, path, "", tenant, fmt.Sprintf(`{"%s":%q,"password":%q}`, field, username, password), status).Data
		if status != http.StatusOK {
			return ""
		}
		var tokens struct{ AccessToken string }
		if err := json.Unmarshal(data, &tokens); err != nil || tokens.AccessToken == "" {
			t.Fatalf("invalid tokens: %v", err)
		}
		return tokens.AccessToken
	}
	id := func(r reply) string {
		t.Helper()
		var data struct{ ID string }
		if err := json.Unmarshal(r.Data, &data); err != nil || data.ID == "" {
			t.Fatalf("missing ID in %s: %v", r.Data, err)
		}
		return data.ID
	}
	const op = "/api/operation/system/"
	const mp = "/api/system/"
	opAdmin := login(operation[0], admin.OperationArea, "bootstrap", admin.OperationTenant, password, 200)
	financeAdmin := login(operation[1], admin.OperationArea, "bootstrap", admin.FinanceTenant, password, 200)
	request(operation[1], op+"user/add", opAdmin, "", fmt.Sprintf(`{"username":"bootstrap","password":%q}`, password), 409)
	menuID := id(request(operation[0], op+"menu/add", opAdmin, "", `{"name":"Moderate","url":"/moderate","menuType":2,"perms":"api:operation:usermanage:user:page"}`, 200))
	roleEditID := id(request(operation[1], op+"menu/add", opAdmin, "", `{"name":"Delegate","hidden":1,"perms":"api:operation:system:role:add"}`, 200))
	roleID := id(request(operation[1], op+"role/add", opAdmin, "", fmt.Sprintf(`{"name":"Support","code":"support","menuIds":[%q,%q],"status":0}`, menuID, roleEditID), 200))
	financeMenu := id(request(operation[1], op+"menu/add", financeAdmin, "", `{"name":"Finance","perms":"api:operation:room:page"}`, 200))
	financeRole := id(request(operation[1], op+"role/add", financeAdmin, "", fmt.Sprintf(`{"name":"Finance","menuIds":[%q]}`, financeMenu), 200))
	if v := request(operation[0], op+"role/detail", opAdmin, "", fmt.Sprintf(`{"id":%q}`, financeRole), 200); v.Status != "4005" {
		t.Fatalf("cross-tenant role: %+v", v)
	}
	userID := id(request(operation[1], op+"user/add", opAdmin, "", fmt.Sprintf(`{"username":"support","nickName":"Support","password":"first-pass","roleIds":[%q]}`, roleID), 200))
	user := login(operation[0], admin.OperationArea, "SUPPORT", admin.OperationTenant, "first-pass", 200)
	login(operation[1], admin.OperationArea, "support", admin.FinanceTenant, "first-pass", 401)
	const page = "/api/operation/usermanage/user/page"
	request(operation[1], page, user, "", "{}", 200)
	if v := request(operation[1], op+"user/info", user, "", "{}", 200); !strings.Contains(string(v.Data), `"name":"Moderate"`) || strings.Contains(string(v.Data), "password") {
		t.Fatalf("route or credential response %s", v.Data)
	}
	request(operation[1], op+"menu/edit", user, "", fmt.Sprintf(`{"id":%q,"name":"Changed","perms":"api:operation:room:page"}`, menuID), 403)
	if v := request(operation[0], op+"role/add", user, "", fmt.Sprintf(`{"name":"Outside","menuIds":[%q]}`, financeMenu), 200); v.Status != "4005" {
		t.Fatalf("cross-tenant grant: %+v", v)
	}
	existingOther := id(request(operation[0], op+"menu/add", opAdmin, "", `{"name":"Other","perms":"api:operation:room:page"}`, 200))
	request(operation[0], op+"role/add", user, "", fmt.Sprintf(`{"name":"Escalation","menuIds":[%q]}`, existingOther), 403)
	request(operation[0], op+"role/add", user, "", fmt.Sprintf(`{"name":"Delegated","menuIds":[%q]}`, menuID), 200)
	request(operation[1], op+"role/edit", opAdmin, "", fmt.Sprintf(`{"id":%q,"name":"Support","code":"support","status":1}`, roleID), 200)
	request(operation[0], page, user, "", "{}", 403)
	request(operation[1], op+"role/edit", opAdmin, "", fmt.Sprintf(`{"id":%q,"name":"Support","code":"support","status":0}`, roleID), 200)
	request(operation[0], page, user, "", "{}", 200)
	request(operation[0], op+"user/changepwd", user, "", `{"oldPassword":"wrong","newPassword":"second-pass"}`, 403)
	request(operation[0], op+"user/changepwd", user, "", `{"oldPassword":"first-pass","newPassword":"second-pass"}`, 200)
	request(operation[1], page, user, "", "{}", 401)
	login(operation[1], admin.OperationArea, "support", admin.OperationTenant, "first-pass", 401)
	user = login(operation[1], admin.OperationArea, "support", admin.OperationTenant, "second-pass", 200)
	request(operation[0], op+"user/edit", opAdmin, "", fmt.Sprintf(`{"id":%q,"nickName":"Support","status":1}`, userID), 200)
	request(operation[1], page, user, "", "{}", 401)
	login(operation[1], admin.OperationArea, "support", admin.OperationTenant, "second-pass", 401)
	request(operation[0], op+"tenant/add", financeAdmin, "", `{"name":"Forbidden","code":"other-admin"}`, 403)
	request(operation[1], op+"tenant/add", opAdmin, "", `{"name":"Created","code":"new-admin","username":"owner","password":"tenant-pass"}`, 200)
	owner := login(operation[0], admin.OperationArea, "owner", "new-admin", "tenant-pass", 200)
	request(operation[0], page, owner, "{}", "{}", 403)

	mcnAdmin := login(mcn[0], admin.MCNArea, "admin-one@example.test", "", password, 200)
	mcnOther := login(mcn[1], admin.MCNArea, "admin-two@example.test", "", password, 200)
	mcnMenu := id(request(mcn[0], mp+"menu/add", mcnAdmin, "", `{"name":"Staff","perms":"api:system:role:group_staff_list"}`, 200))
	request(mcn[1], mp+"role/add", mcnAdmin, "", fmt.Sprintf(`{"name":"Team","menuIds":[%q]}`, mcnMenu), 200)
	groupList := request(mcn[0], mp+"role/list", mcnAdmin, "", "", 200)
	var groups []struct{ ID string }
	if err := json.Unmarshal(groupList.Data, &groups); err != nil || len(groups) != 1 {
		t.Fatalf("group list %s: %v", groupList.Data, err)
	}
	mcnRole := groups[0].ID
	mcnUser := id(request(mcn[0], mp+"user/add", mcnAdmin, "", fmt.Sprintf(`{"name":"Staff","email":"staff@example.test","passwd":"staff-pass","ty":3,"mcnId":"10","roleIds":[%q]}`, mcnRole), 200))
	staff := login(mcn[1], admin.MCNArea, "staff@example.test", "", "staff-pass", 200)
	request(mcn[0], mp+"role/set_group_admin", mcnAdmin, "", fmt.Sprintf(`{"id":%q,"userId":%q}`, mcnRole, mcnUser), 200)
	if v := request(mcn[1], mp+"role/group_staff_list", staff, "", fmt.Sprintf(`{"id":%q}`, mcnRole), 200); !strings.Contains(string(v.Data), `"groupAdmin":1`) {
		t.Fatalf("group membership: %+v", v)
	}
	if v := request(mcn[0], mp+"user/detail", mcnOther, "", fmt.Sprintf(`{"id":%q}`, mcnUser), 200); v.Status != "4005" {
		t.Fatalf("cross-agency account: %+v", v)
	}
	if v := request(mcn[0], mp+"role/detail", mcnOther, "", fmt.Sprintf(`{"id":%q}`, mcnRole), 200); v.Status != "4005" {
		t.Fatalf("cross-agency role: %+v", v)
	}
	request(mcn[1], mp+"role/delete", mcnAdmin, "", fmt.Sprintf(`{"ids":[%q]}`, mcnRole), 200)
	request(mcn[0], mp+"role/group_staff_list", staff, "", fmt.Sprintf(`{"id":%q}`, mcnRole), 403)
	request(mcn[1], mp+"user/delete", mcnAdmin, "", fmt.Sprintf(`{"ids":[%q]}`, mcnUser), 200)
	request(mcn[0], mp+"user/get_routes", staff, "", "{}", 401)
}
