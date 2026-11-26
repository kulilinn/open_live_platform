package app_test

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/sha3"
	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/collectible"
	"open_live_platform/internal/store/postgres"
)

func TestCollectibleAndAdorn(t *testing.T) {
	testCollectibleAndAdorn(t, app.Config{})
}

func TestPostgresCollectibleAndAdorn(t *testing.T) {
	config, _ := postgresTestDatabase(t)
	testCollectibleAndAdorn(t, config)
}

func testCollectibleAndAdorn(t *testing.T, config app.Config) {
	t.Helper()
	ctx := t.Context()
	const (
		contract = "0xffffffffffffffffffffffffffffffffffffffff"
		wallet   = "0x1111111111111111111111111111111111111111"
		buyer    = "0x2222222222222222222222222222222222222222"
	)
	signature := strings.Repeat("01", 64) + "1b"
	var rpcOwner, rpcSigner, expectedData atomic.Value
	rpcOwner.Store(wallet)
	rpcSigner.Store(wallet)
	expectedData.Store("")
	var rpcCalls atomic.Int64
	chain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rpcCalls.Add(1)
		var req struct {
			ID     int
			Method string
			Params []json.RawMessage
		}
		var call struct{ To, Data string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Method != "eth_call" ||
			len(req.Params) != 2 || string(req.Params[1]) != `"latest"` ||
			json.Unmarshal(req.Params[0], &call) != nil {
			http.Error(w, "wrong RPC payload", http.StatusBadRequest)
			return
		}
		var address string
		switch call.To {
		case "0x0000000000000000000000000000000000000001":
			if call.Data != expectedData.Load().(string) {
				t.Errorf("ecrecover calldata = %s, want %s", call.Data, expectedData.Load())
				http.Error(w, "wrong ecrecover calldata", http.StatusBadRequest)
				return
			}
			address = rpcSigner.Load().(string)
		case contract:
			if call.Data != fmt.Sprintf("0x6352211e%064x", uint32(4294967295)) &&
				call.Data != fmt.Sprintf("0x6352211e%064x", uint32(4294967294)) {
				t.Errorf("ownerOf calldata = %s", call.Data)
				http.Error(w, "wrong ownerOf calldata", http.StatusBadRequest)
				return
			}
			address = rpcOwner.Load().(string)
		default:
			t.Errorf("unexpected target %s", call.To)
			http.Error(w, "wrong target", http.StatusBadRequest)
			return
		}
		fmt.Fprintf(w, `{"id":%d,"jsonrpc":"2.0","result":"0x%024d%s"}`, req.ID, 0, address[2:])
	}))
	t.Cleanup(chain.Close)
	catalog := filepath.Join(t.TempDir(), "nft.json")
	if err := os.WriteFile(catalog, []byte(fmt.Sprintf(`{
		"cards":[{"id":4294967295,"status":1,"walletAddress":%q,"cardEnglishName":"Genesis"},{"id":30,"status":2}],
		"privileges":[{"id":1,"rightsName":"Entrance"}],
		"adorns":[{"id":3,"unlockCondition":1,"outfitName":"Genesis","entryEffect":"spark","effectTime":5,"exclusiveAvatarFrame":"blue"},
		          {"id":4,"unlockCondition":1,"outfitName":"Alternative","entryEffect":"glow","effectTime":9},
		          {"id":5,"unlockCondition":2,"outfitName":"Unverified"}]}`, wallet)), 0600); err != nil {
		t.Fatal(err)
	}
	config.Collectibles = collectible.Config{RPC: chain.URL, Contract: contract, CatalogFile: catalog,
		DefaultPhoto: "default.png", BoundPhoto: "nft.png"}
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
	first, second := signUp("nft-first"), signUp("nft-second")
	makeRequest := func(method, path, token, body string, code int, status string) json.RawMessage {
		t.Helper()
		r := httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		platform.Server.Handler.ServeHTTP(w, r)
		var out struct {
			Status string
			Data   json.RawMessage
		}
		err := json.Unmarshal(w.Body.Bytes(), &out)
		if err != nil || w.Code != code || out.Status != status {
			t.Fatalf("%s %s = %d %s, decode=%v", method, path, w.Code, w.Body.String(), err)
		}
		return out.Data
	}
	const nft = "/api/nft/creation_card/"
	const adorn = "/api/adorn/"
	page := makeRequest("POST", nft+"page", "", `{"status":[1],"page":"1","pageSize":1}`, 200, "200")
	if !strings.Contains(string(page), `"walletAddress":"0x1111****1111"`) || !strings.Contains(string(page), `"cardEnglishName":"Genesis"`) ||
		strings.Contains(string(page), wallet) || !strings.Contains(string(page), `"total":1`) {
		t.Fatalf("unexpected public card response %s", page)
	}
	makeRequest("GET", nft+"get_challenge", "", "", 401, "")
	data := makeRequest("GET", nft+"get_privilege_list", first.Token, "", 200, "200")
	if !strings.Contains(string(data), `"rightsName":"Entrance"`) {
		t.Fatalf("missing privilege: %s", data)
	}
	data = makeRequest("GET", adorn+"get_list?adornType=1", first.Token, "", 200, "200")
	if strings.Contains(string(data), "Unverified") || !strings.Contains(string(data), "Alternative") || strings.Contains(string(data), "effectTime") {
		t.Fatalf("unexpected adorn configuration: %s", data)
	}
	makeRequest("POST", adorn+"equip", first.Token, `{"adornId":3}`, 200, "4091")
	challenge := func(token string) int64 {
		t.Helper()
		data := makeRequest("GET", nft+"get_challenge", token, "", 200, "200")
		var result collectible.Challenge
		if err := json.Unmarshal(data, &result); err != nil || result.ID <= 0 || len(result.Message) != 64 {
			t.Fatalf("invalid challenge %s: %v", data, err)
		}
		hash := sha3.NewLegacyKeccak256()
		hash.Write([]byte("\x19Ethereum Signed Message:\n64" + result.Message))
		expectedData.Store("0x" + hex.EncodeToString(hash.Sum(nil)) + fmt.Sprintf("%064x", 27) + signature[:128])
		return result.ID
	}
	bind := func(token string, id int64, account, sig, want string) {
		t.Helper()
		body := fmt.Sprintf(`{"challengeId":"%d","account":%q,"signature":%q,"tokenId":4294967295}`, id, account, sig)
		makeRequest("POST", nft+"binding", token, body, 200, want)
	}
	id := challenge(first.Token)
	bind(second.Token, id, wallet, signature, "4087")
	bind(first.Token, id, wallet, "01", "4087")
	if rpcCalls.Load() != 0 {
		t.Fatal("invalid challenge or signature reached the chain")
	}
	bind(first.Token, id, buyer, signature, "4087")
	rpcOwner.Store(buyer)
	bind(first.Token, id, wallet, signature, "4089")
	rpcOwner.Store(wallet)
	if config.DatabaseURL != "" {
		platform = open()
	}
	bind(first.Token, id, wallet, "0x"+signature, "200")
	bind(first.Token, id, wallet, signature, "4087")
	bind(first.Token, challenge(first.Token), wallet, signature, "4088")
	checkView := func(token string, wantBound bool, expected string) {
		t.Helper()
		raw := makeRequest("GET", nft+"get_binding", token, "", 200, "200")
		var view collectible.View
		if err := json.Unmarshal(raw, &view); err != nil || (view.TokenID != nil) != wantBound ||
			(view.TokenID != nil && *view.TokenID != 4294967295) || view.Equipped["1"].Value != expected {
			t.Fatalf("unexpected NFT state %s, err=%v", raw, err)
		}
	}
	checkView(first.Token, true, "spark")
	makeRequest("POST", adorn+"unequip_by_type", first.Token, `{"adornType":1}`, 200, "200")
	checkView(first.Token, true, "")
	makeRequest("POST", adorn+"equip", first.Token, `{"adornId":4}`, 200, "200")
	makeRequest("POST", adorn+"equip", first.Token, `{"adornId":5}`, 200, "4091")
	makeRequest("POST", adorn+"equip", first.Token, `{"adornId":6}`, 200, "4005")
	checkView(first.Token, true, "glow")
	profile := makeRequest("GET", "/api/user/me", first.Token, "", 200, "200")
	if !strings.Contains(string(profile), `"token_id":4294967295`) ||
		!strings.Contains(string(profile), `"value":"glow","duration":9`) ||
		!strings.Contains(string(profile), `"value":"blue"`) || !strings.Contains(string(profile), `"photo":"nft.png"`) {
		t.Fatalf("profile did not include equipped items: %s", profile)
	}
	rpcOwner.Store(buyer)
	rpcSigner.Store(buyer)
	bind(second.Token, challenge(second.Token), buyer, signature, "200")
	checkView(first.Token, false, "")
	profile = makeRequest("GET", fmt.Sprintf("/api/user/%d", first.User.ID), second.Token, "", 200, "200")
	if !strings.Contains(string(profile), `"photo":"default.png"`) {
		t.Fatalf("profile should restore default photo: %s", profile)
	}
	checkView(second.Token, true, "spark")
	rpcOwner.Store(wallet)
	checkView(second.Token, false, "")
	makeRequest("POST", nft+"unbinding", second.Token, "", 200, "4090")
	rpcSigner.Store(wallet)
	bind(first.Token, challenge(first.Token), wallet, signature, "200")
	makeRequest("POST", nft+"unbinding", first.Token, "", 200, "200")
	checkView(first.Token, false, "")
	if config.DatabaseURL != "" {
		third := signUp("legacy-nft")
		store, err := postgres.Open(ctx, config.DatabaseURL)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(store.Shutdown)
		chainService, err := collectible.New(config.Collectibles, store)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := chainService.VerifyOwnership(ctx, 4294967295, buyer); err != collectible.ErrNotOwned {
			t.Fatalf("stale owner accepted during import: %v", err)
		}
		binding, err := chainService.VerifyOwnership(ctx, 4294967295, wallet)
		if err != nil {
			t.Fatal(err)
		}
		added, err := store.ImportLegacyNFT(ctx, third.User.ID, binding, map[int8]int32{1: 3})
		if err != nil || added {
			t.Fatalf("legacy import overwrote newer ownership history: added=%t err=%v", added, err)
		}
		binding, err = chainService.VerifyOwnership(ctx, 4294967294, wallet)
		if err != nil {
			t.Fatal(err)
		}
		added, err = store.ImportLegacyNFT(ctx, third.User.ID, binding, map[int8]int32{1: 4})
		if err != nil || !added {
			t.Fatalf("could not import the verified NFT: added=%t err=%v", added, err)
		}
		raw := makeRequest("GET", nft+"get_binding", third.Token, "", 200, "200")
		if !strings.Contains(string(raw), `"tokenId":4294967294`) || !strings.Contains(string(raw), `"value":"glow"`) {
			t.Fatalf("legacy NFT and selected adorn not preserved: %s", raw)
		}
		makeRequest("POST", nft+"unbinding", third.Token, "", 200, "200")
		added, err = store.ImportLegacyNFT(ctx, third.User.ID, binding, map[int8]int32{1: 4})
		if err != nil || added {
			t.Fatalf("legacy import restored historical unbind: added=%t err=%v", added, err)
		}
	}
}
