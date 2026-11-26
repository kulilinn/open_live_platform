package app_test

import (
	"encoding/json"
	"net/http/httptest"
	"open_live_platform/internal/app"
	"open_live_platform/internal/catalog"
	"open_live_platform/internal/store/postgres"
	"strings"
	"testing"
)

func TestPublicCatalog(t *testing.T) {
	var data catalog.Snapshot
	err := json.Unmarshal([]byte(`{
		"apps":[{"app_id":123,"state":3,"popularity":1,"release_time":"2026-01-01T00:00:00.5Z","app_secret":"redacted"},{"app_id":124,"state":2,"popularity":5,"release_time":"2026-01-01T00:00:00Z"},{"app_id":125,"state":1}],
		"countries":[{"code":"JP","name":"Japan"}],"versions":[{"name":"ios","curr_version":"1.23.0"}],
		"marquee":[{"id":93000000000000005,"content":"live","jump_url":null,"name":"News","is_jump":0}],
		"classify":[{"id":1,"pid":0,"name":"Help","language":"en"},{"id":2,"pid":1,"name":"Chat","language":"en"},{"id":1,"pid":0,"name":"ヘルプ","language":"ja"}],
		"questions":[{"id":93000000000000005,"first_classify_id":1,"second_classify_id":2,"language":"en","is_hot":1,"question":"How?"}],
		"answers":[{"id":93000000000000005,"language":"en","answer":"Go live."}]
	}`), &data)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"memory", "postgres"} {
		t.Run(mode, func(t *testing.T) {
			cfg := app.Config{PublicCatalog: data}
			if mode == "postgres" {
				cfg, _ = postgresTestDatabase(t)
				db, err := postgres.Open(t.Context(), cfg.DatabaseURL)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Shutdown()
				for range 2 {
					if err := db.ReplacePublicCatalog(t.Context(), data); err != nil {
						t.Fatal(err)
					}
				}
			}
			a, err := app.New(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			test := func(method, path, body string, want ...string) string {
				t.Helper()
				w := httptest.NewRecorder()
				a.Server.Handler.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
				if w.Code != 200 {
					t.Fatalf("%s: %d %s", path, w.Code, w.Body)
				}
				for _, check := range want {
					if !strings.Contains(w.Body.String(), check) {
						t.Fatalf("%s: %s missing %s", path, w.Body, check)
					}
				}
				return w.Body.String()
			}
			first := test("POST", "/api/app/list", "{}", `"app_id":123`)
			if strings.Contains(first, "app_secret") || strings.Contains(first, "125") || strings.Index(first, "123") > strings.Index(first, "124") {
				t.Fatal(first)
			}
			ranked := test("POST", "/api/app/list", `{"offset":1,"order_type":"Popularity"}`, `"app_id":123`)
			if strings.Contains(ranked, "124") {
				t.Fatal(ranked)
			}
			test("GET", "/countries/XX", "", `"code":"JP"`)
			test("POST", "/check_version/ios", "", `"curr_version":"1.23.0"`)
			test("POST", "/check_version/no", "", `"status":"4005"`)
			test("GET", "/api/marquee/list", "", `"id":"93000000000000005"`, `"jumpUrl":null`)
			test("GET", "/api/help/classifylist?language=ja", "", "ヘルプ")
			test("GET", "/api/help/classifydetail?language=en&firstClassifyId=1", "", "Chat", "How?", `"id":"93000000000000005"`)
			test("GET", "/api/help/hotquestionlist?language=ja", "", `"data":[]`)
			test("GET", "/api/help/getanswer?language=en&questionId=93000000000000005&secondClassifyId=2", "", "Go live.", `"secondClassifyName":"Chat"`, `"firstClassifyName":null`)
		})
	}
}
