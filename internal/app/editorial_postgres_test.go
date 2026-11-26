package app_test

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"golang.org/x/crypto/argon2"
	"open_live_platform/internal/admin"
	"open_live_platform/internal/app"
)

type editorialAPI struct {
	operator, public        http.Handler
	platform                *app.App
	token, tenant, password string
}

func startEditorialAPI(t *testing.T, configure ...func(*app.Config)) editorialAPI {
	t.Helper()
	cfg, _ := postgresTestDatabase(t)
	password := rand.Text()
	salt := []byte("editorial-test-salt")
	digest := argon2.IDKey([]byte(password), salt, 2, 4096, 1, 32)
	hash := fmt.Sprintf("$argon2id$v=19$m=4096,t=2,p=1$%s$%s", base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(digest))
	data, err := json.Marshal([]admin.Principal{
		{ID: 97, Area: admin.OperationArea, Tenant: admin.OperationTenant, Username: "editor", Admin: true, PasswordHash: hash},
		{ID: 98, Area: admin.OperationArea, Tenant: admin.FinanceTenant, Username: "finance", Admin: true, PasswordHash: hash},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg.AdminAddr = "127.0.0.1:0"
	for _, apply := range configure {
		apply(&cfg)
	}
	cfg.AdminAccountsFile = filepath.Join(t.TempDir(), "operators.json")
	if err := os.WriteFile(cfg.AdminAccountsFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	start := func() *app.App {
		p, err := app.New(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		return p
	}
	platform := start()
	api := editorialAPI{operator: start().Backoffice.Handler, public: platform.Server.Handler, platform: platform, tenant: admin.OperationTenant, password: password}
	var creds struct{ AccessToken string }
	api.call(t, "/api/auth/accountlogin", map[string]any{"username": "editor", "password": password}, 200, &creds)
	if creds.AccessToken == "" {
		t.Fatal("missing operator token")
	}
	api.token = creds.AccessToken
	return api
}

func (a editorialAPI) call(t *testing.T, path string, input any, code int, output any) {
	t.Helper()
	handler, method := a.operator, http.MethodPost
	if len(path) > 0 && path[0] == '@' {
		path = path[1:]
		handler = a.public
		if input == nil {
			method = http.MethodGet
		}
	}
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequestWithContext(t.Context(), method, path, bytes.NewReader(body))
	if a.token != "" {
		r.Header.Set("Authorization", "Bearer "+a.token)
	}
	r.Header.Set("X-Loverose-Admin", a.tenant)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	var result struct {
		Status string
		Data   json.RawMessage
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != code || result.Status != strconv.Itoa(code) {
		t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
	}
	if output != nil {
		if err := json.Unmarshal(result.Data, output); err != nil {
			t.Fatalf("%s: %v, data=%s", path, err, result.Data)
		}
	}
}

func TestPostgresHelpAndVersions(t *testing.T) {
	api := startEditorialAPI(t)
	const cat = "/api/operation/help/category/"
	const question = "/api/operation/help/question/"
	const version = "/api/operation/platform/version/"
	type object = map[string]any
	anonymous := api
	anonymous.token = ""
	anonymous.call(t, cat+"add", object{"pid": "0", "name": "ignored", "language": "en"}, 401, nil)
	addCategory := func(parent, name string) string {
		t.Helper()
		api.call(t, cat+"add", object{"pid": parent, "name": name, "language": "en", "sort": 1}, 200, nil)
		var list []struct{ ID, Name, PID string }
		api.call(t, cat+"list", object{"pid": parent, "language": "en"}, 200, &list)
		if len(list) != 1 || list[0].Name != name || list[0].PID != parent {
			t.Fatalf("category list: %+v", list)
		}
		return list[0].ID
	}
	first := addCategory("0", "Streams")
	second := addCategory(first, "Chat")
	categoryID := object{"id": second, "language": "en"}
	item := object{"firstClassifyId": first, "secondClassifyId": second, "title": "Where is chat?", "status": 1, "language": "en", "content": "Open the room", "isHot": 1, "sort": 2}
	api.call(t, question+"add", item, 200, nil)
	api.call(t, question+"add", item, 400, nil)
	var page struct {
		List []struct {
			ID                string
			FirstID, SecondID int64
		}
		Total int
	}
	api.call(t, question+"page", object{"classifyId": first, "title": "Where", "language": "en", "page": 1, "pageSize": 2}, 200, &page)
	if len(page.List) != 1 || page.Total != 1 || fmt.Sprint(page.List[0].FirstID) != first || fmt.Sprint(page.List[0].SecondID) != second {
		t.Fatalf("question page: %+v", page)
	}
	id := page.List[0].ID
	var details object
	api.call(t, "@/api/help/getanswer?language=en&questionId="+id, nil, 200, &details)
	if details["answer"] != "Open the room" {
		t.Fatalf("public answer: %v", details)
	}
	api.call(t, cat+"delete", categoryID, 400, nil)
	item["id"], item["content"] = id, "Enter the stream"
	api.call(t, question+"edit", item, 200, nil)
	idQuery := object{"id": id, "language": "en"}
	var answer string
	api.call(t, question+"get_answer", idQuery, 200, &answer)
	if answer != "Enter the stream" {
		t.Fatal(answer)
	}
	api.call(t, question+"change_status", idQuery, 200, nil)
	api.call(t, question+"detail", idQuery, 200, &details)
	if details["content"] != answer || details["status"] != 0.0 {
		t.Fatal(details)
	}
	api.call(t, question+"delete", idQuery, 200, nil)
	var hot []object
	api.call(t, "@/api/help/hotquestionlist?language=en", nil, 200, &hot)
	if len(hot) != 0 {
		t.Fatal(hot)
	}
	api.call(t, cat+"delete", categoryID, 200, nil)
	v := object{"name": "mobile", "platform": 1, "currVersion": "3.1", "contentEn": "Release", "iosUrl": "https://mobile.test"}
	api.call(t, version+"add_or_edit", v, 200, nil)
	v["currVersion"] = "3.2"
	api.call(t, version+"add_or_edit", v, 200, nil)
	var mobile object
	api.call(t, "@/check_version/mobile", object{}, 200, &mobile)
	if mobile["curr_version"] != "3.2" {
		t.Fatal(mobile)
	}
	var releases []object
	api.call(t, version+"page", nil, 200, &releases)
	if len(releases) != 1 || releases[0]["currVersion"] != "3.2" {
		t.Fatal(releases)
	}
	api.call(t, version+"delete", object{"id": "mobile"}, 200, nil)
	api.call(t, version+"page", nil, 200, &releases)
	if len(releases) != 0 {
		t.Fatal(releases)
	}
}

func TestPostgresBroadcastPublishing(t *testing.T) {
	api := startEditorialAPI(t)
	const config = "/api/operation/broadcast/config/"
	const marquee = "/api/operation/broadcast/marquee/"
	type object = map[string]any
	row := object{"id": "99012345678901234", "name": "Gifts", "format": 1, "jumpType": 1, "background": "banner"}
	api.call(t, config+"add", row, 200, nil)
	api.call(t, config+"add", row, 400, nil)
	row["jumpName"] = "Room"
	api.call(t, config+"edit", row, 200, nil)
	var configs struct{ List []object }
	api.call(t, config+"page", object{"name": "Gifts"}, 200, &configs)
	if len(configs.List) != 1 || configs.List[0]["id"] != row["id"] || configs.List[0]["jumpName"] != "Room" {
		t.Fatal(configs)
	}
	message := object{"name": "News", "contentKey": "welcome", "isJump": 0, "status": 0, "displayOrder": 2}
	api.call(t, marquee+"add", message, 200, nil)
	var public []object
	api.call(t, "@/api/marquee/list", nil, 200, &public)
	if len(public) != 0 {
		t.Fatal(public)
	}
	var page struct{ List []object }
	api.call(t, marquee+"page", object{}, 200, &page)
	if len(page.List) != 1 || page.List[0]["contentKey"] != "welcome" {
		t.Fatal(page)
	}
	message["id"], message["status"] = page.List[0]["id"], 1
	api.call(t, marquee+"edit", message, 200, nil)
	api.call(t, "@/api/marquee/list", nil, 200, &public)
	if len(public) != 1 || public[0]["content"] != "welcome" || public[0]["id"] != message["id"] {
		t.Fatal(public)
	}
	api.call(t, marquee+"delete", object{"id": message["id"]}, 200, nil)
	api.call(t, "@/api/marquee/list", nil, 200, &public)
	if len(public) != 0 {
		t.Fatal(public)
	}
}
