package app_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/argon2"
	"open_live_platform/internal/admin"
	"open_live_platform/internal/anchor"
	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
)

func TestPostgresAnchorOnboarding(t *testing.T) {
	config, db := postgresTestDatabase(t)
	ctx := t.Context()
	password := "admin-test-password"
	salt := []byte("test-salt-asdf12")
	key := argon2.IDKey([]byte(password), salt, 2, 4096, 1, 32)
	hash := fmt.Sprintf("$argon2id$v=19$m=4096,t=2,p=1$%s$%s", base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
	principals, err := json.Marshal([]admin.Principal{
		{ID: 10, Area: admin.OperationArea, Tenant: admin.OperationTenant, Username: "reviewer", PasswordHash: hash, Admin: true},
		{ID: 11, Area: admin.OperationArea, Tenant: admin.OperationTenant, Username: "readonly", PasswordHash: hash,
			Permissions: []string{"api:operation:business:anchorreview:page"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	config.AdminAddr = "127.0.0.1:0"
	config.AdminAccountsFile = filepath.Join(t.TempDir(), "account.json")
	if err := os.WriteFile(config.AdminAccountsFile, principals, 0600); err != nil {
		t.Fatal(err)
	}
	reviewer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var item struct{ Kind, Text string }
		if r.Header.Get("Authorization") != "Bearer review-sandbox" || json.NewDecoder(r.Body).Decode(&item) != nil ||
			!strings.HasPrefix(item.Kind, "anchor_") {
			http.Error(w, "invalid audit", 400)
			return
		}
		if item.Text == "rejected input" {
			fmt.Fprint(w, `{"pass_status":2}`)
		} else {
			fmt.Fprint(w, `{"pass_status":3}`)
		}
	}))
	defer reviewer.Close()
	config.PostAuditURL, config.PostAuditToken = reviewer.URL, "review-sandbox"
	var activeID atomic.Int64
	var green atomic.Bool
	var createCalls atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hash := hmac.New(sha256.New, []byte("verification-test-key"))
		hash.Write([]byte(r.Header.Get("X-App-Access-Ts") + r.Method + r.URL.RequestURI()))
		hash.Write(body)
		if r.Header.Get("X-App-Token") != "verification-test-token" ||
			r.Header.Get("X-App-Access-Sig") != hex.EncodeToString(hash.Sum(nil)) {
			http.Error(w, "wrong verification signature", 401)
			return
		}
		id := strconv.FormatInt(activeID.Load(), 10)
		switch r.Method + " " + r.URL.Path {
		case "POST /resources/applicants":
			var applicant struct{ ExternalUserID string }
			if err := json.Unmarshal(body, &applicant); err != nil || applicant.ExternalUserID != id || r.URL.Query().Get("levelName") != "anchor sandbox" {
				http.Error(w, "invalid applicant", 400)
				return
			}
			createCalls.Add(1)
			fmt.Fprintf(w, `{"id":"verification-a","externalUserId":%q}`, id)
		case "POST /resources/accessTokens":
			if r.URL.Query().Get("userId") != id {
				http.Error(w, "invalid token account", 400)
				return
			}
			fmt.Fprintf(w, `{"userId":%q,"token":"sandbox-token"}`, id)
		case "GET /resources/applicants/verification-a/one":
			answer := "RED"
			if green.Load() {
				answer = "GREEN"
			}
			fmt.Fprintf(w, `{"id":"verification-a","externalUserId":%q,"review":{"reviewStatus":"completed","reviewResult":{"reviewAnswer":%q}}}`, id, answer)
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	config.KYC = anchor.KYCConfig{URL: provider.URL, Token: "verification-test-token", Secret: "verification-test-key", Level: "anchor sandbox"}
	platform, err := app.New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(platform.Close)
	newUser := func(name string) auth.Session {
		t.Helper()
		s, err := platform.Accounts.SignUp(ctx, name+"@example.test", "user-password", name)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	alice, bob := newUser("Alice"), newUser("TakenName")
	activeID.Store(alice.User.ID)
	public, ops := platform.Server.Handler, platform.Backoffice.Handler
	type reply struct {
		Status       string
		Data, Result json.RawMessage
	}
	post := func(handler http.Handler, path, token, body string, code int, status string) reply {
		t.Helper()
		r := httptest.NewRequestWithContext(ctx, "POST", path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Loverose-Admin", admin.OperationTenant)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		var value reply
		if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil || w.Code != code || value.Status != status {
			t.Fatalf("%s -> %d %s, err=%v", path, w.Code, w.Body.String(), err)
		}
		return value
	}
	const api = "/anchor/api/"
	const adminAPI = "/api/operation/business/anchorreview/"
	id := strconv.FormatInt(alice.User.ID, 10)
	profileRequest := fmt.Sprintf(`{"anchor_id":%q}`, id)
	reviewRequest := fmt.Sprintf(`{"id":%q}`, id)
	if raw := post(public, api+"is_anchor", alice.Token, "", 200, "200").Result; string(raw) != `{"is_anchor":false}` {
		t.Fatalf("ordinary user status = %s", raw)
	}
	post(public, api+"become_anchor", alice.Token, `{"country":"JP","family_name":"Private","address":"Hidden"}`, 200, "200")
	post(public, api+"become_anchor", alice.Token, `{"twitter":"handle"}`, 200, "200")
	post(public, api+"get_info", bob.Token, profileRequest, 200, "4005")
	post(public, api+"set_socialize_info", alice.Token, `{"twitter_link":"alice"}`, 401, "401")
	if raw := post(public, api+"kyc_get_token", alice.Token, "", 200, "200").Data; !strings.Contains(string(raw), `"token":"sandbox-token"`) {
		t.Fatalf("provider token = %s", raw)
	}
	post(public, api+"kyc_get_token", alice.Token, "", 200, "200")
	if createCalls.Load() != 1 {
		t.Fatalf("multiple provider applicants %d", createCalls.Load())
	}
	if raw := post(public, api+"kyc_set_info", alice.Token, `{"verified":true,"idCard":"client-claim"}`, 200, "200").Data; string(raw) != "false" {
		t.Fatalf("client KYC claims were trusted: %s", raw)
	}
	green.Store(true)
	if raw := post(public, api+"kyc_set_info", alice.Token, `{}`, 200, "200").Data; string(raw) != "true" {
		t.Fatalf("provider KYC was not recorded: %s", raw)
	}
	login := func(name string) string {
		t.Helper()
		result := post(ops, "/api/auth/accountlogin", "", fmt.Sprintf(`{"username":%q,"password":%q}`, name, password), 200, "200")
		var token struct{ AccessToken string }
		if err := json.Unmarshal(result.Data, &token); err != nil || token.AccessToken == "" {
			t.Fatalf("reviewer credentials = %s / %v", result.Data, err)
		}
		return token.AccessToken
	}
	readonly, operator := login("readonly"), login("reviewer")
	if result := post(ops, adminAPI+"page", readonly, reviewRequest, 200, "200").Data; !strings.Contains(string(result), `"kycStatus":1`) ||
		!strings.Contains(string(result), `"state":0`) {
		t.Fatalf("pending review list = %s", result)
	}
	post(ops, adminAPI+"pass", readonly, reviewRequest, 403, "403")
	post(ops, adminAPI+"pass", operator, reviewRequest, 200, "200")
	post(ops, adminAPI+"pass", operator, reviewRequest, 200, "200")
	if raw := post(public, api+"is_anchor", alice.Token, "", 200, "200").Result; !strings.Contains(string(raw), `"is_anchor":true`) {
		t.Fatalf("approved status = %s", raw)
	}
	if result := post(public, api+"get_info", bob.Token, profileRequest, 200, "200").Result; strings.Contains(string(result), "Hidden") ||
		strings.Contains(string(result), "Private") || !strings.Contains(string(result), `"country":"JP"`) {
		t.Fatalf("unsafe profile %s", result)
	}
	post(public, api+"set_info", alice.Token, `{"display_name":"Anchor Alice","search_name":"takenname"}`, 200, "4032")
	post(public, api+"set_info", alice.Token, `{"display_name":"Anchor Alice","search_name":"AliceShow","intro":"rejected input"}`, 200, "4067")
	post(public, api+"set_info", alice.Token, `{"display_name":"Anchor Alice","search_name":"AliceShow","intro":"Intro","img":"https://example.test/avatar.png","trans":true,"trans_lang":"ja"}`, 200, "200")
	post(public, api+"set_socialize_info", alice.Token, `{"twitter_link":"newhandle","yutube_link":"@anchor"}`, 200, "200")
	if result := post(public, api+"get_info", bob.Token, profileRequest, 200, "200").Result; !strings.Contains(string(result), `"display_name":"Anchor Alice"`) ||
		!strings.Contains(string(result), `"twitter":"newhandle"`) || !strings.Contains(string(result), `"trans_lang":"ja"`) {
		t.Fatalf("updated profile = %s", result)
	}
	var count int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM subscription_plans WHERE anchor_id=$1 AND plan_type IN(2,3)`, alice.User.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("source default plans should occur once: %d / %v", count, err)
	}
	post(ops, adminAPI+"reject", operator, fmt.Sprintf(`{"id":%q,"reason":"manual judgment"}`, id), 200, "200")
	if raw := post(public, api+"is_anchor", alice.Token, "", 200, "200").Result; !strings.Contains(string(raw), `"reject_reason":"manual judgment"`) {
		t.Fatalf("rejected state = %s", raw)
	}
	post(public, api+"get_info", bob.Token, profileRequest, 200, "4005")
	if err := db.QueryRow(ctx, `SELECT count(*) FROM backoffice_logs WHERE target=$1 AND operator_id=10
		AND action IN ('anchor_approved','anchor_rejected')`, id).Scan(&count); err != nil || count != 3 {
		t.Fatalf("persistent reviews audit = %d, error=%v", count, err)
	}
}
