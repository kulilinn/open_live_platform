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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"open_live_platform/internal/admin"
	"open_live_platform/internal/app"
	"open_live_platform/internal/collectible"
	"open_live_platform/internal/store/postgres"
)

func TestPostgresNFTBackofficeAcrossReplicas(t *testing.T) {
	config, db := postgresTestDatabase(t)
	const contract = "0x2222222222222222222222222222222222222222"
	const owner = "0x3a33333333333333333333333333333333333333"
	const other = "0x4444444444444444444444444444444444444444"
	var calls atomic.Int64
	var unavailable atomic.Bool
	var changedOwner atomic.Bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if unavailable.Load() {
			http.Error(w, "fixture offline", http.StatusBadGateway)
			return
		}
		var req struct {
			ID     int
			Method string
			Params []json.RawMessage
		}
		var call struct{ To, Data string }
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.Method != "eth_call" || len(req.Params) != 2 ||
			json.Unmarshal(req.Params[0], &call) != nil || call.To != contract || string(req.Params[1]) != `"latest"` ||
			(call.Data != fmt.Sprintf("0x6352211e%064x", 7) && call.Data != fmt.Sprintf("0x6352211e%064x", 8)) {
			http.Error(w, "unexpected ownerOf request", http.StatusBadRequest)
			return
		}
		account := owner
		if changedOwner.Load() {
			account = other
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": "0x" + strings.Repeat("0", 24) + account[2:]})
	}))
	t.Cleanup(provider.Close)
	hash, err := admin.SystemPassword("managed-nft-fixture-password")
	if err != nil {
		t.Fatal(err)
	}
	principals := admin.Principals{
		{ID: 500, Area: admin.OperationArea, Tenant: admin.OperationTenant, Username: "owner", Admin: true, PasswordHash: hash},
		{ID: 501, Area: admin.OperationArea, Tenant: admin.FinanceTenant, Username: "finance", Admin: true, PasswordHash: hash},
		{ID: 502, Area: admin.OperationArea, Tenant: admin.OperationTenant, Username: "reader", PasswordHash: hash,
			Permissions: []string{"api:operation:nft:creation_card:page"}},
	}
	credential, err := json.Marshal(principals)
	if err != nil {
		t.Fatal(err)
	}
	config.AdminAccountsFile = filepath.Join(t.TempDir(), "admin.json")
	if err := os.WriteFile(config.AdminAccountsFile, credential, 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "nft.json")
	if err := os.WriteFile(path, []byte(`{"cards":[{"id":7,"status":5,"cardEnglishName":"Genesis"}],
		"adorns":[{"id":1,"unlockCondition":1,"entryEffect":"entrance"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	config.AdminAddr = "127.0.0.1:0"
	config.Collectibles = collectible.Config{RPC: provider.URL, Contract: contract, CatalogFile: path}
	var replicas [2]*app.App
	var adminURLs [2]string
	for i := range replicas {
		p, err := app.New(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		srv := httptest.NewServer(p.Backoffice.Handler)
		t.Cleanup(srv.Close)
		replicas[i], adminURLs[i] = p, srv.URL
	}
	second, err := replicas[1].Accounts.SignUp(t.Context(), "nft-target-two@example.test", "nft-test-pass", "Two")
	if err != nil {
		t.Fatal(err)
	}
	first, err := replicas[0].Accounts.SignUp(t.Context(), "nft-target-one@example.test", "nft-test-pass", "One")
	if err != nil {
		t.Fatal(err)
	}
	type response struct {
		Status string
		Data   json.RawMessage
	}
	send := func(address, path, token, tenant, key, body string) (response, int, error) {
		var reply response
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, address+path, strings.NewReader(body))
		if err != nil {
			return reply, 0, err
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if tenant != "" {
			req.Header.Set("X-Loverose-Admin", tenant)
		}
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return reply, 0, err
		}
		defer res.Body.Close()
		if res.StatusCode == http.StatusNotFound {
			_, err = io.Copy(io.Discard, res.Body)
			return reply, res.StatusCode, err
		}
		err = json.NewDecoder(res.Body).Decode(&reply)
		return reply, res.StatusCode, err
	}
	check := func(address, path, token, key, body string, wantHTTP int, wantStatus string) response {
		t.Helper()
		reply, code, err := send(address, path, token, "", key, body)
		if err != nil || code != wantHTTP || reply.Status != wantStatus {
			t.Fatalf("%s = %d %+v, %v; want %d %s", path, code, reply, err, wantHTTP, wantStatus)
		}
		return reply
	}
	login := func(address, tenant, user string) string {
		t.Helper()
		reply, code, err := send(address, "/api/auth/accountlogin", "", tenant, "", fmt.Sprintf(`{"username":%q,"password":"managed-nft-fixture-password"}`, user))
		var tokens struct{ AccessToken string }
		if err != nil || code != 200 || json.Unmarshal(reply.Data, &tokens) != nil || tokens.AccessToken == "" {
			t.Fatalf("login %s: %d %+v %v", user, code, reply, err)
		}
		return tokens.AccessToken
	}
	operator := login(adminURLs[0], admin.OperationTenant, "owner")
	finance := login(adminURLs[1], admin.FinanceTenant, "finance")
	reader := login(adminURLs[1], admin.OperationTenant, "reader")
	const prefix = "/api/operation/nft/creation_card/"
	public := httptest.NewServer(replicas[0].Server.Handler)
	t.Cleanup(public.Close)
	binding := func(user int64, token int, address string) string {
		return fmt.Sprintf(`{"userId":"%d","tokenId":%d,"account":%q}`, user, token, address)
	}
	unbinding := func(user int64) string { return fmt.Sprintf(`{"userId":"%d"}`, user) }
	check(public.URL, prefix+"binding", operator, "", binding(first.User.ID, 7, owner), 404, "")
	check(adminURLs[0], prefix+"page", "", "", "{}", 401, "401")
	check(adminURLs[1], prefix+"page", finance, "", "{}", 403, "403")
	check(adminURLs[0], prefix+"binding", reader, "", binding(first.User.ID, 7, owner), 403, "403")
	if calls.Load() != 0 {
		t.Fatal("unauthorized request reached ownerOf")
	}
	check(adminURLs[0], prefix+"binding", operator, "fixture-bad-owner", binding(first.User.ID, 7, other), 200, "4089")
	check(adminURLs[0], prefix+"binding", operator, "fixture-missing-user", binding(9000000000000, 7, owner), 200, "4005")

	var attempts sync.WaitGroup
	for _, address := range adminURLs {
		attempts.Add(1)
		go func() {
			defer attempts.Done()
			v, code, err := send(address, prefix+"binding", operator, "", "first-bind", binding(first.User.ID, 7, owner))
			if err != nil || code != 200 || v.Status != "200" {
				t.Errorf("simultaneous bind returned %d %+v %v", code, v, err)
			}
		}()
	}
	attempts.Wait()
	page := check(adminURLs[1], prefix+"page", reader, "", `{"id":7,"status":[5],"page":1,"pageSize":5}`, 200, "200")
	if !strings.Contains(string(page.Data), fmt.Sprintf(`"userId":"%d"`, first.User.ID)) || !strings.Contains(string(page.Data), owner) {
		t.Fatalf("bound inventory %s", page.Data)
	}
	check(adminURLs[1], prefix+"binding", operator, "", binding(first.User.ID, 8, owner), 200, "4088")
	check(adminURLs[1], prefix+"binding", operator, "wrong-owner-retry", binding(second.User.ID, 7, other), 200, "4089")
	check(adminURLs[1], prefix+"binding", operator, "chain-rpc-retry", binding(second.User.ID, 7, other), 200, "4089")
	unavailable.Store(true)
	check(adminURLs[1], prefix+"binding", operator, "first-bind", binding(first.User.ID, 7, strings.ToUpper(owner)), 200, "200")
	check(adminURLs[1], prefix+"binding", operator, "first-bind", binding(second.User.ID, 7, owner), 409, "409")
	check(adminURLs[1], prefix+"binding", operator, "chain-rpc-retry", binding(second.User.ID, 7, other), 503, "503")
	unavailable.Store(false)
	changedOwner.Store(true)
	check(adminURLs[0], prefix+"binding", operator, "chain-rpc-retry", binding(second.User.ID, 7, other), 200, "200")

	check(adminURLs[0], prefix+"unbinding", operator, "removed-first", unbinding(first.User.ID), 200, "200")
	check(adminURLs[1], prefix+"unbinding", operator, "removed-second", unbinding(second.User.ID), 200, "200")
	check(adminURLs[0], prefix+"unbinding", operator, "removed-second", unbinding(second.User.ID), 200, "200")
	check(adminURLs[0], prefix+"binding", operator, "confirmed-rebind", binding(second.User.ID, 7, other), 200, "200")
	check(adminURLs[0], prefix+"unbinding", operator, "removed-second", unbinding(second.User.ID), 200, "200")
	var activeID int64
	var activity, slots int
	err = db.QueryRow(t.Context(), `SELECT (SELECT user_id FROM nft_bindings WHERE token_id=7),
		(SELECT count(*) FROM nft_binding_events WHERE token_id=7),(SELECT count(*) FROM nft_equipped_adorn)`).Scan(&activeID, &activity, &slots)
	if err != nil || activeID != second.User.ID || activity != 5 || slots != 1 {
		t.Fatalf("binding %d vs %d, history %d, slots %d, error %v", activeID, second.User.ID, activity, slots, err)
	}

	check(adminURLs[0], prefix+"edit", reader, "", `{"id":7,"status":3}`, 403, "403")
	check(adminURLs[0], prefix+"edit", operator, "edit-7", `{"id":7,"status":3,"name":"Reserved","accountName":"Customer","description":"fixture"}`, 200, "200")
	check(adminURLs[1], prefix+"edit", operator, "edit-7", `{"id":7,"status":5}`, 409, "409")
	check(adminURLs[1], prefix+"edit", operator, "", `{"id":4242,"status":5}`, 200, "4005")
	check(adminURLs[1], prefix+"update_sale_time", operator, "batch-retry", `{"id":[7,8],"saleTime":"2026-01-01 12:30:00"}`, 200, "4005")
	var sale *time.Time
	if err := db.QueryRow(t.Context(), `SELECT sale_at FROM nft_card_inventory WHERE id=7`).Scan(&sale); err != nil || sale != nil {
		t.Fatalf("unmatched batch partially committed: %v %v", sale, err)
	}
	target, err := postgres.Open(t.Context(), config.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(target.Shutdown)
	now := time.Now().UTC()
	cards := []collectible.ManagedCard{{ID: 8, English: "Historic", Status: 2, Created: &now}, {ID: 7, English: "Stale import", Status: 5, Created: &now}}
	if added, err := target.ImportLegacyNFTInventory(t.Context(), cards); err != nil || added != 1 {
		t.Fatalf("first card import %d %v", added, err)
	}
	if added, err := target.ImportLegacyNFTInventory(t.Context(), cards); err != nil || added != 0 {
		t.Fatalf("replay card import %d %v", added, err)
	}
	check(adminURLs[1], prefix+"update_sale_time", operator, "batch-retry", `{"id":[8,7,8],"saleTime":"2026-01-01 12:30:00"}`, 200, "200")
	check(adminURLs[0], prefix+"update_sale_time", operator, "batch-retry", `{"id":[7,8],"saleTime":"2026-01-01T12:30:00Z"}`, 200, "200")
	page = check(adminURLs[0], prefix+"page", operator, "", `{"status":[2,3],"pageSize":"2"}`, 200, "200")
	if !strings.Contains(string(page.Data), `"preOrderName":"Customer"`) || !strings.Contains(string(page.Data), `"cardEnglishName":"Historic"`) ||
		strings.Count(string(page.Data), `"saleTime":"2026-01-01T12:30:00Z"`) != 2 {
		t.Fatalf("persisted inventory %s", page.Data)
	}
}
