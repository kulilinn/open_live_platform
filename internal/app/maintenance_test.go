package app_test

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaintenanceRequiresOperatorAndPreservesOrders(t *testing.T) {
	cfg, db := postgresTestDatabase(t)
	ctx := t.Context()
	cfg.MaintenanceToken = "operators-only"
	cfg.AgoraAppID = strings.Repeat("1", 32)
	cfg.AgoraCertificate = strings.Repeat("2", 32)
	cfg.DemoItemsFile = filepath.Join(t.TempDir(), "demo.json")
	if err := os.WriteFile(cfg.DemoItemsFile, []byte(`[{"id":7,"name":"test","count":2}]`), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := app.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	call := func(method, path, token, body string, code int) string {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		a.Server.Handler.ServeHTTP(w, r)
		if w.Code != code {
			t.Fatalf("%s %s: %d %s, want %d", method, path, w.Code, w.Body, code)
		}
		return w.Body.String()
	}
	tkn := cfg.MaintenanceToken
	if got := call("GET", "/testconfig", "", "", 200); !strings.Contains(got, `"len":1`) {
		t.Fatal(got)
	}
	call("POST", "/api/fill_search_name", "", "", 403)
	call("POST", "/api/fill_search_name", tkn, "", 200)
	body := `{"users":[{"email":"anchor@example.org","password":"original","displayName":"creator","isAnchor":true},{"email":"fan@example.org","password":"pass","displayName":"fan"}]}`
	call("POST", "/batch_signup", tkn, body, 200)
	call("POST", "/batch_signup", tkn, strings.ReplaceAll(body, "original", "changed"), 200)
	anchor, err := a.Accounts.SignIn(ctx, "anchor@example.org", "original")
	if err != nil {
		t.Fatal(err)
	}
	if profile := auth.PublicProfile(anchor.User); !profile.IsAnchor || !strings.HasPrefix(profile.SearchName, "u") {
		t.Fatalf("batch profile %+v", profile)
	}
	fan, err := a.Accounts.SignIn(ctx, "fan@example.org", "pass")
	if err != nil {
		t.Fatal(err)
	}
	var plans int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM subscription_plans WHERE anchor_id=$1`, anchor.User.ID).Scan(&plans); err != nil || plans != 2 {
		t.Fatalf("plans %d %v", plans, err)
	}
	call("POST", "/batch_signup", tkn, `{"users":[{"email":"rolled@example.org","password":"pw","displayName":"rollback"},{"email":"bad"}]}`, 400)
	if _, err := a.Accounts.SignIn(ctx, "rolled@example.org", "pw"); err == nil {
		t.Fatal("partial batch")
	}
	room, err := a.Rooms.Start(ctx, anchor.User.ID, "on air", "")
	if err != nil {
		t.Fatal(err)
	}
	route := fmt.Sprintf("/api/live/adtmz3gw6u5fnh4qrxsm/%d/1/%d/%d", room.ID, room.Session.AnchorNID, anchor.User.ID)
	call("GET", route, "", "", 403)
	var signed struct{ Data struct{ Token string } }
	if err := json.Unmarshal([]byte(call("GET", route, tkn, "", 200)), &signed); err != nil || signed.Data.Token == "" {
		t.Fatal("missing signed RTC token")
	}
	call("GET", fmt.Sprintf("/api/live/adtmz3gw6u5fnh4qrxsm/%d/1/%d/%d", room.ID, room.Session.AnchorNID, fan.User.ID), tkn, "", 403)
	del := "/anchor/api/delete_anchor/anchor@example.org"
	call("GET", del, fan.Token, "", 403)
	if _, err := db.Exec(ctx, `INSERT INTO payment_large_order_attempts(user_id,request_key,provider_order) VALUES($1,'history','paid')`, anchor.User.ID); err != nil {
		t.Fatal(err)
	}
	call("GET", del, tkn, "", 409)
	if r, err := a.Rooms.Info(ctx, room.ID); err != nil || !r.Online {
		t.Fatal("unsuccessful delete closed room", err)
	}
	call("GET", "/anchor/api/delete_anchor/fan@example.org", tkn, "", 200)
	if _, found, err := a.Accounts.Lookup(ctx, fan.Token); err != nil || found {
		t.Fatal("deleted account session is live", err)
	}
	call("GET", "/anchor/api/delete_anchor/fan@example.org", tkn, "", 404)
}
