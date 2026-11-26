package app_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"open_live_platform/internal/app"
	"open_live_platform/internal/collectible"
)

func TestPostgresNFTInventoryRefresh(t *testing.T) {
	config, _ := postgresTestDatabase(t)
	ctx := t.Context()
	const treasury = "0x1111111111111111111111111111111111111111"
	const contract = "0x2222222222222222222222222222222222222222"
	const buyer = "0x3333333333333333333333333333333333333333"
	var treasuryOwns atomic.Bool
	var failOneOwner atomic.Bool
	var calls atomic.Int64
	treasuryOwns.Store(true)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request struct {
			ID     int
			Method string
			Params []json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Params) != 2 {
			http.Error(w, "invalid RPC request", 400)
			return
		}
		var result any
		switch request.Method {
		case "eth_getBlockByNumber":
			if string(request.Params[0]) != `"finalized"` || string(request.Params[1]) != "false" {
				http.Error(w, "expected finalized block", 400)
				return
			}
			result = map[string]string{"number": "0x10"}
		case "eth_call":
			var params struct{ To, Data string }
			if json.Unmarshal(request.Params[0], &params) != nil || params.To != contract || string(request.Params[1]) != `"0x10"` {
				http.Error(w, "expected contract at pinned block", 400)
				return
			}
			switch params.Data {
			case "0x8462151c000000000000000000000000" + treasury[2:]:
				if treasuryOwns.Load() {
					result = fmt.Sprintf("0x%064x%064x%064x", 32, 1, 10)
				} else {
					result = fmt.Sprintf("0x%064x%064x", 32, 0)
				}
			case fmt.Sprintf("0x6352211e%064x", 7):
				result = fmt.Sprintf("0x%024d%s", 0, buyer[2:])
			case fmt.Sprintf("0x6352211e%064x", 10):
				if failOneOwner.Load() {
					http.Error(w, "sandbox failure", 502)
					return
				}
				result = fmt.Sprintf("0x%024d%s", 0, buyer[2:])
			default:
				t.Errorf("unexpected calldata %s", params.Data)
				http.Error(w, "unknown contract selector", 400)
				return
			}
		default:
			t.Errorf("unexpected RPC method %s", request.Method)
			http.Error(w, "unexpected RPC method", 400)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	t.Cleanup(provider.Close)
	path := filepath.Join(t.TempDir(), "nft.json")
	if err := os.WriteFile(path, []byte(`{"cards":[{"id":7,"status":3,"cardEnglishName":"Reserved"}],
		"inventory":[{"id":10,"cardEnglishName":"Minted"},{"id":11,"cardEnglishName":"Unminted"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	config.Collectibles = collectible.Config{RPC: provider.URL, Contract: contract, CatalogFile: path,
		Treasury: treasury, SyncToken: "nft-test-sync"}
	open := func() *app.App {
		t.Helper()
		a, err := app.New(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(a.Close)
		return a
	}
	a := open()
	request := func(route, auth, body string, status int) string {
		t.Helper()
		r := httptest.NewRequestWithContext(ctx, "POST", "/api/nft/creation_card/"+route, strings.NewReader(body))
		r.Header.Set("Authorization", auth)
		w := httptest.NewRecorder()
		a.Server.Handler.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s = HTTP %d %s", route, w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	syncAuth := "Bearer " + config.Collectibles.SyncToken
	request("schedule_refresh", "", "", 401)
	if calls.Load() != 0 {
		t.Fatal("unauthorized request reached blockchain provider")
	}
	request("init_data_20250523", syncAuth, "", 200)
	request("init_data_20250523", syncAuth, "", 200)
	body := request("page", "", `{"status":[5]}`, 200)
	if !strings.Contains(body, `"cardEnglishName":"Minted"`) ||
		strings.Contains(body, "Unminted") || !strings.Contains(body, `"total":1`) {
		t.Fatalf("initialized cards = %s", body)
	}
	treasuryOwns.Store(false)
	failOneOwner.Store(true)
	request("schedule_refresh", syncAuth, "", 503)
	body = request("page", "", `{"status":[1]}`, 200)
	if !strings.Contains(body, `"total":0`) {
		t.Fatalf("failed chain check advanced inventory = %s", body)
	}
	failOneOwner.Store(false)
	request("schedule_refresh", syncAuth, "", 200)
	a = open()
	body = request("page", "", `{"status":[1]}`, 200)
	if !strings.Contains(body, `"total":2`) || strings.Count(body, `"walletAddress":"0x3333****3333"`) != 2 ||
		strings.Contains(body, buyer) {
		t.Fatalf("confirmed cards = %s", body)
	}
}
