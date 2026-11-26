package app_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/argon2"
	"open_live_platform/internal/admin"
	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/live"
)

func TestPostgresBackofficeListenersAndModeration(t *testing.T) {
	config, sql := postgresTestDatabase(t)
	ctx := t.Context()
	freeAddr := func() string {
		t.Helper()
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		return listener.Addr().String()
	}
	config.Addr, config.GRPCAddr, config.AdminAddr, config.MCNAdminAddr = freeAddr(), freeAddr(), freeAddr(), freeAddr()
	config.RelaySecret = "backoffice-test"
	password := rand.Text()
	var salt [16]byte
	rand.Read(salt[:])
	key := argon2.IDKey([]byte(password), salt[:], 2, 4096, 1, 32)
	hash := fmt.Sprintf("$argon2id$v=19$m=4096,t=2,p=1$%s$%s", base64.RawStdEncoding.EncodeToString(salt[:]), base64.RawStdEncoding.EncodeToString(key))
	principals := []admin.Principal{
		{ID: 101, Area: admin.OperationArea, Tenant: admin.OperationTenant, Username: "operations", PasswordHash: hash, Admin: true},
		{ID: 102, Area: admin.OperationArea, Tenant: admin.OperationTenant, Username: "reader", PasswordHash: hash,
			Permissions: []string{"api:operation:usermanage:user:page"}},
		{ID: 103, Area: admin.OperationArea, Tenant: "finance-admin", Username: "finance", PasswordHash: hash, Admin: true},
		{ID: 201, Area: admin.MCNArea, AgencyID: 9, Email: "manager@example.test", PasswordHash: hash, Manager: true,
			Permissions: []string{"api:streamer:page", "api:streamer:list"}},
		{ID: 202, Area: admin.MCNArea, AgencyID: 8, Email: "other-mcn@example.test", PasswordHash: hash, Admin: true},
	}
	credentials, err := json.Marshal(principals)
	if err != nil {
		t.Fatal(err)
	}
	config.AdminAccountsFile = filepath.Join(t.TempDir(), "backoffice.json")
	if err := os.WriteFile(config.AdminAccountsFile, credentials, 0600); err != nil {
		t.Fatal(err)
	}
	config.GiftCatalogFile = filepath.Join(t.TempDir(), "gift-catalog.json")
	if err := os.WriteFile(config.GiftCatalogFile, []byte(`[{"id":"9007199254740993","name":"Rose","price":12,"giftType":1}]`), 0600); err != nil {
		t.Fatal(err)
	}
	platform, err := app.New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(platform.Close)
	signUp := func(name string) auth.Session {
		t.Helper()
		session, err := platform.Accounts.SignUp(ctx, name+"@example.test", "test-password", name)
		if err != nil {
			t.Fatal(err)
		}
		return session
	}
	host, viewer, other := signUp("host"), signUp("viewer"), signUp("other")
	room, err := platform.Rooms.Start(ctx, host.User.ID, "Broadcast", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := platform.Rooms.Join(ctx, room.ID, viewer.User.ID, ""); err != nil {
		t.Fatal(err)
	}
	_, err = sql.Exec(ctx, `INSERT INTO gift_orders (room_id,live_id,sender_id,receiver_id,request_key,payment_order,request,item,sender_info,receiver_info,
		sender_role,cost,income,state,paid_at,provider_order_id,receipt)
		VALUES($1,$2,$3,$1,'fixture','provider-fixture','{"gift_id":"9007199254740993","quantity":1}','{"name":"Rose","icon":"rose.png"}',
		'{"name":"viewer"}','{"name":"host"}',2,12,8,1,now(),40,'{"order_no":"9007199254740994","total_bad_debt_point":3}')`,
		room.ID, room.Session.LiveID, viewer.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = sql.Exec(ctx, `INSERT INTO backoffice_mcn_streamers (id,mcn_id,streamer_id,manager_id,email,name,ratio,manager_ratio,agency_ratio,status,create_time)
		VALUES(301,9,$1,201,'host@example.test','Host',0.4,0.1,0.5,0,now()),(302,9,$2,204,'viewer@example.test','Viewer',0.4,0.1,0.5,0,now()),
		(303,8,$3,202,'other@example.test','Other',0.4,0.1,0.5,0,now())`, host.User.ID, viewer.User.ID, other.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- platform.Run(serveCtx) }()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("run platform: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("management listeners did not shut down")
		}
	})
	client := &http.Client{Timeout: 5 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	for _, address := range []string{config.AdminAddr, config.MCNAdminAddr, config.Addr} {
		deadline := time.Now().Add(5 * time.Second)
		for {
			response, err := client.Get("http://" + address + "/healthchecker")
			if err == nil {
				response.Body.Close()
				break
			}
			if !time.Now().Before(deadline) {
				t.Fatalf("listener %s did not start: %v", address, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	type result struct {
		Status, URL string
		Data        json.RawMessage
	}
	request := func(address, method, path, token, tenant, body string, want int) result {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, "http://"+address+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if tenant != "" {
			req.Header.Set("X-Loverose-Admin", tenant)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != want {
			t.Fatalf("%s %s = %d, want %d", method, path, res.StatusCode, want)
		}
		var out result
		if want != http.StatusNotFound && res.StatusCode != http.StatusNoContent {
			if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
				t.Fatal(err)
			}
		}
		return out
	}
	type tokens struct {
		AccessToken, RefreshToken string
		ExpiresIn                 int
	}
	login := func(address, endpoint, body, tenant string) tokens {
		t.Helper()
		out := request(address, http.MethodPost, endpoint, "", tenant, body, http.StatusOK)
		var trusted tokens
		if err := json.Unmarshal(out.Data, &trusted); err != nil || out.Status != "200" || trusted.AccessToken == "" || trusted.RefreshToken == "" || trusted.ExpiresIn != 7200 {
			t.Fatalf("admin login result = status %q, err %v", out.Status, err)
		}
		return trusted
	}
	adminLoginBody := func(name string) string { return fmt.Sprintf(`{"username":%q,"password":%q}`, name, password) }
	const apiLogin = "/api/auth/accountlogin"
	request(config.Addr, http.MethodPost, apiLogin, "", admin.OperationTenant, adminLoginBody("operations"), http.StatusNotFound)
	request(config.AdminAddr, http.MethodPost, apiLogin, "", "finance-admin", adminLoginBody("operations"), http.StatusUnauthorized)
	operations := login(config.AdminAddr, apiLogin, adminLoginBody("operations"), admin.OperationTenant)
	reader := login(config.AdminAddr, apiLogin, adminLoginBody("reader"), admin.OperationTenant)
	finance := login(config.AdminAddr, apiLogin, adminLoginBody("finance"), "finance-admin")
	mcn := login(config.MCNAdminAddr, "/api/user/account_login", fmt.Sprintf(`{"email":"manager@example.test","password":%q}`, password), "")
	const accounts = "/api/operation/usermanage/user/"
	giftPage := request(config.AdminAddr, http.MethodPost, "/api/operation/gifts/gift/page", operations.AccessToken, "", `{"name":"Rose","ty":1}`, http.StatusOK)
	if !strings.Contains(string(giftPage.Data), `"id":"9007199254740993"`) || !strings.Contains(string(giftPage.Data), `"price":"12"`) {
		t.Fatalf("gift catalog page = %s", giftPage.Data)
	}
	const purchases = "/api/finance/purchase/page"
	request(config.AdminAddr, http.MethodPost, purchases, operations.AccessToken, "", `{}`, http.StatusForbidden)
	purchase := request(config.AdminAddr, http.MethodPost, purchases, finance.AccessToken, "", `{"tradeNo":"9007199254740994","tradeTy":2,"pageSize":"1"}`, http.StatusOK)
	if !strings.Contains(string(purchase.Data), `"total":1`) || !strings.Contains(string(purchase.Data), `"badDebtLd":2`) || !strings.Contains(string(purchase.Data), `"point":12`) {
		t.Fatalf("purchase page = %s", purchase.Data)
	}
	request(config.AdminAddr, http.MethodPost, accounts+"page", mcn.AccessToken, "", `{}`, http.StatusUnauthorized)
	request(config.AdminAddr, http.MethodPost, accounts+"page", viewer.Token, "", `{}`, http.StatusUnauthorized)
	request(config.AdminAddr, http.MethodPost, accounts+"page", finance.AccessToken, "", `{}`, http.StatusForbidden)
	request(config.AdminAddr, http.MethodPost, accounts+"detail", reader.AccessToken, "", `{"account":"1"}`, http.StatusForbidden)
	page := request(config.AdminAddr, http.MethodPost, accounts+"page", reader.AccessToken, "", `{"page":"1","pageSize":"1","nickName":"viewer"}`, http.StatusOK)
	if !strings.Contains(string(page.Data), `"total":1`) || !strings.Contains(string(page.Data), `"displayName":"viewer"`) {
		t.Fatalf("filtered account page = %s", page.Data)
	}
	mcnPage := request(config.MCNAdminAddr, http.MethodPost, "/api/streamer/page", mcn.AccessToken, "", `{"mcnId":8,"managerId":"204"}`, http.StatusOK)
	if !strings.Contains(string(mcnPage.Data), `"id":"301"`) || strings.Contains(string(mcnPage.Data), `"id":"302"`) || strings.Contains(string(mcnPage.Data), `"id":"303"`) {
		t.Fatalf("MCN manager read outside assignment = %s", mcnPage.Data)
	}
	request(config.Addr, http.MethodGet, "/api/admin/users", operations.AccessToken, "", "", http.StatusUnauthorized)
	const refresh = "/api/auth/refreshtoken"
	request(config.AdminAddr, http.MethodPost, refresh, operations.AccessToken, "", "", http.StatusUnauthorized)
	newSession := request(config.AdminAddr, http.MethodPost, refresh, operations.RefreshToken, "", "", http.StatusOK)
	request(config.AdminAddr, http.MethodPost, refresh, operations.RefreshToken, "", "", http.StatusUnauthorized)
	if err := json.Unmarshal(newSession.Data, &operations); err != nil {
		t.Fatal(err)
	}
	viewerID, roomID := strconv.FormatInt(viewer.User.ID, 10), strconv.FormatInt(room.ID, 10)
	chatBody := fmt.Sprintf(`{"room_id":%d,"text":"hello"}`, room.ID)
	ticket := request(config.Addr, http.MethodGet, "/api/device/qrcode", viewer.Token, "", "", http.StatusOK)
	dialer := websocket.Dialer{Subprotocols: []string{"h5relayer"}, HandshakeTimeout: 5 * time.Second}
	conn, _, err := dialer.DialContext(ctx, ticket.URL+"?room_id="+roomID, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var greeting struct{ Content string }
	if err := conn.ReadJSON(&greeting); err != nil || greeting.Content != "control" {
		t.Fatalf("H5 greeting=%v err=%v", greeting, err)
	}
	operation := func(path, body string) result {
		return request(config.AdminAddr, http.MethodPost, path, operations.AccessToken, "", body, http.StatusOK)
	}
	operation(accounts+"no_talking", `{"userId":"`+viewerID+`","periodTime":60,"createByName":"spoofed","reason":["spam"]}`)
	request(config.Addr, http.MethodPost, "/api/live/room/chat", viewer.Token, "", chatBody, http.StatusForbidden)
	operation(accounts+"account_disable", `{"userId":"`+viewerID+`","periodTime":-1,"reason":["harassment"]}`)
	if _, _, err = conn.ReadMessage(); !websocket.IsCloseError(err, websocket.CloseNormalClosure) || !strings.Contains(err.Error(), "SERVICE_LOGOUT") {
		t.Fatalf("banned H5 connection err=%v", err)
	}
	request(config.Addr, http.MethodGet, "/api/user/me", viewer.Token, "", "", http.StatusUnauthorized)
	request(config.Addr, http.MethodPost, "/api/auth/signin", "", "", `{"email":"viewer@example.test","password":"test-password"}`, http.StatusForbidden)
	violations := operation(accounts+"violation_records", `{"id":"`+viewerID+`","page":1,"pageSize":2}`)
	if !strings.Contains(string(violations.Data), `"total":2`) || strings.Contains(string(violations.Data), "spoofed") {
		t.Fatalf("violation records = %s", violations.Data)
	}
	operation(accounts+"account_enable", `{"userId":"`+viewerID+`"}`)
	request(config.Addr, http.MethodPost, "/api/auth/signin", "", "", `{"email":"viewer@example.test","password":"test-password"}`, http.StatusOK)
	const roomPrefix = "/api/operation/room/"
	roomPage := operation(roomPrefix+"page", `{"roomId":"`+roomID+`"}`)
	if !strings.Contains(string(roomPage.Data), `"onlineStatus":1`) {
		t.Fatalf("room page = %s", roomPage.Data)
	}
	operation(roomPrefix+"streamer_op", `{"ty":"Disable","streamerId":"`+roomID+`","roomId":"`+roomID+`","reason":["test"]}`)
	roomStatus, err := platform.Rooms.Info(ctx, room.ID)
	if err != nil || roomStatus.Status() != live.RoomLimited {
		t.Fatalf("limited room=%+v err=%v", roomStatus, err)
	}
	limited := request(config.Addr, http.MethodPost, "/api/live/room/start?compat=rust", host.Token, "", `{"subject":"New"}`, http.StatusOK)
	if limited.Status != "4035" {
		t.Fatalf("limited start = %s", limited.Status)
	}
	operation(roomPrefix+"streamer_op", `{"ty":"Enable","streamerId":"`+roomID+`","roomId":"`+roomID+`"}`)
	if started := request(config.Addr, http.MethodPost, "/api/live/room/start?compat=rust", host.Token, "", `{"subject":"New"}`, http.StatusOK); started.Status != "200" {
		t.Fatalf("enabled start = %s", started.Status)
	}
	logs := operation("/api/system/user/operationlogpage", `{"page":1,"pageSize":20}`)
	if !strings.Contains(string(logs.Data), `"total":5`) || !strings.Contains(string(logs.Data), `"createBy":"101"`) {
		t.Fatalf("audit logs=%s", logs.Data)
	}
	operation("/api/auth/logout", "")
	request(config.AdminAddr, http.MethodPost, accounts+"page", operations.AccessToken, "", `{}`, http.StatusUnauthorized)
}
