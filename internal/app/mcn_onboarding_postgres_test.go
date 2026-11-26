package app_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
	"open_live_platform/internal/admin"
	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/notice"
	"open_live_platform/internal/store/postgres"
)

func TestPostgresMCNOnboarding(t *testing.T) {
	config, db := postgresTestDatabase(t)
	ctx := t.Context()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	const password = "local-onboarding-test"
	salt := []byte("test-salt-asdf12")
	hash := fmt.Sprintf("$argon2id$v=19$m=4096,t=2,p=1$%s$%s",
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(argon2.IDKey([]byte(password), salt, 2, 4096, 1, 32)))
	principals := []admin.Principal{
		{ID: 21, Area: admin.MCNArea, AgencyID: 21, Email: "admin-one@example.test", Admin: true, PasswordHash: hash},
		{ID: 22, Area: admin.MCNArea, AgencyID: 22, Email: "admin-two@example.test", Admin: true, PasswordHash: hash},
		{ID: 23, Area: admin.MCNArea, AgencyID: 21, Email: "manager@example.test", Manager: true, PasswordHash: hash,
			Permissions: []string{"api:streamer:add", "api:streamer:edit", "api:streamer:account_new"}},
	}
	payload, err := json.Marshal(principals)
	check(err)
	config.MCNAdminAddr = "127.0.0.1:0"
	config.MCNJoinURL = "https://mcn.example.test/api/mcn/streamer/join"
	config.AdminAccountsFile = filepath.Join(t.TempDir(), "staff.json")
	check(os.WriteFile(config.AdminAccountsFile, payload, 0600))
	config.GiftCatalogFile = filepath.Join(t.TempDir(), "catalog.json")
	check(os.WriteFile(config.GiftCatalogFile, []byte(`[{"id":"87","name":"Settlement gift","price":30,"incomeStream":8}]`), 0600))
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Point   int64
			OrderNo string `json:"order_no"`
		}
		if r.Method != "POST" || r.URL.Path != "/api/payment/point/decrease" || json.NewDecoder(r.Body).Decode(&input) != nil || input.Point != 30 {
			http.Error(w, "unexpected debit", 400)
			return
		}
		fmt.Fprint(w, `{"code":200,"data":{"order_id":"9007199254741201","order_no":"mcn-payment-confirmed","total_bad_debt_point":15,"total_point":30}}`)
	}))
	t.Cleanup(provider.Close)
	config.PaymentHost, config.PaymentAppID = provider.URL, "mcn-settlement-test"
	a, err := app.New(ctx, config)
	check(err)
	t.Cleanup(a.Close)
	creator, err := a.Accounts.SignUp(ctx, "creator@example.test", "creator-password", "Creator")
	check(err)
	viewer, err := a.Accounts.SignUp(ctx, "viewer@example.test", "creator-password", "Viewer")
	check(err)
	_, err = db.Exec(ctx, "UPDATE users SET profile=$2 WHERE id=$1", creator.User.ID, auth.Profile{Name: "Creator", IsAnchor: true})
	check(err)
	public, mcn := a.Server.Handler, a.MCNBackoffice.Handler
	request := func(handler http.Handler, method, path, token, body string, code int, status string) json.RawMessage {
		t.Helper()
		r := httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		var response struct {
			Status string
			Data   json.RawMessage
		}
		check(json.Unmarshal(w.Body.Bytes(), &response))
		if w.Code != code || response.Status != status {
			t.Fatalf("%s = %d %s, want %d %s", path, w.Code, w.Body.String(), code, status)
		}
		return response.Data
	}
	login := func(email string) string {
		t.Helper()
		reply := request(mcn, "POST", "/api/user/account_login", "", fmt.Sprintf(`{"email":%q,"password":%q}`, email, password), 200, "200")
		var tokens struct{ AccessToken string }
		check(json.Unmarshal(reply, &tokens))
		return tokens.AccessToken
	}
	one, two, manager := login(principals[0].Email), login(principals[1].Email), login(principals[2].Email)
	const route = "/api/streamer/"
	request(mcn, "POST", route+"add", one, `{"name":"Viewer","email":"viewer@example.test","ratio":"0.7"}`, 200, "4005")
	request(mcn, "POST", route+"add", manager, `{"name":"Creator","email":"creator@example.test","ratio":"0.7","mcnId":"22"}`, 200, "200")
	var row int64
	check(db.QueryRow(ctx, `SELECT id FROM backoffice_mcn_streamers WHERE mcn_id=21 AND streamer_id=$1 AND status=1 AND manager_id=23`,
		creator.User.ID).Scan(&row))
	id := strconv.FormatInt(row, 10)
	streamer := strconv.FormatInt(creator.User.ID, 10)
	mutate := fmt.Sprintf(`{"id":%q,"streamerId":%q}`, id, streamer)
	request(mcn, "POST", route+"edit", two, fmt.Sprintf(`{"id":%q,"name":"Spoof","ratio":"0.1"}`, id), 200, "4005")
	request(mcn, "POST", route+"edit", manager, fmt.Sprintf(`{"id":%q,"name":"Creator Stage","ratio":"0.65","managerRatio":"0.05"}`, id), 200, "200")
	request(mcn, "POST", route+"account_new", manager, mutate, 403, "403")
	request(mcn, "POST", route+"account_new", one, mutate, 409, "409")
	request(mcn, "POST", route+"update_status", one, fmt.Sprintf(`{"id":%q,"streamerId":%q,"status":2}`, id, streamer), 200, "200")
	request(mcn, "POST", route+"add", two, `{"name":"Creator Other","email":"creator@example.test","ratio":"0.5"}`, 200, "200")
	list := request(public, "GET", "/api/notice/page_notice?page=1&pageSize=20", creator.Token, "", 200, "200")
	var notices notice.Page
	check(json.Unmarshal(list, &notices))
	links := map[string]*url.URL{}
	for _, n := range notices.List {
		if n.Type != 33 {
			continue
		}
		var fields map[string]string
		check(json.Unmarshal(n.Content, &fields))
		link, err := url.Parse(fields["agree_uri"])
		check(err)
		links[fields["mcn_id"]] = link
	}
	if links["21"] == nil || links["22"] == nil {
		t.Fatalf("missing invitations %s", list)
	}
	mailStore, err := postgres.Open(ctx, config.DatabaseURL)
	check(err)
	t.Cleanup(mailStore.Shutdown)
	mailer := &testMCNMailer{}
	check(admin.DeliverMCNInviteMail(ctx, mailStore, mailer))
	check(admin.DeliverMCNInviteMail(ctx, mailStore, mailer))
	check(admin.DeliverMCNInviteMail(ctx, mailStore, mailer))
	if len(mailer.sent) != 2 || !strings.Contains(strings.Join(mailer.sent, " "), links["21"].String()) ||
		!strings.Contains(strings.Join(mailer.sent, " "), links["22"].String()) {
		t.Fatalf("incorrect invitation delivery: %d", len(mailer.sent))
	}
	if leaked := request(public, "GET", "/api/notice/page_notice?page=1&pageSize=20", viewer.Token, "", 200, "200"); strings.Contains(string(leaked), "agree_uri") {
		t.Fatalf("foreign invitation leaked: %s", leaked)
	}
	request(mcn, "GET", links["21"].Path+"invalid", "", "", 403, "403")
	request(mcn, "GET", links["21"].Path, "", "", 200, "200")
	request(mcn, "GET", links["21"].Path, "", "", 403, "403")
	request(mcn, "GET", links["22"].Path, "", "", 403, "403")
	request(mcn, "POST", route+"account_new", one, mutate, 200, "200")
	var agency int64
	var status int
	check(db.QueryRow(ctx, `SELECT a.agency_id,m.status FROM anchor_profiles a JOIN backoffice_mcn_streamers m ON m.streamer_id=a.user_id AND m.mcn_id=a.agency_id
		WHERE a.user_id=$1`, creator.User.ID).Scan(&agency, &status))
	if agency != 21 || status != 6 {
		t.Fatalf("verified MCN activation = %d/%d", agency, status)
	}
	var audits int
	check(db.QueryRow(ctx, `SELECT COUNT(*) FROM backoffice_logs WHERE target=$1 AND tenant='21' AND action IN
		('mcn_add','mcn_edit','mcn_update_status','mcn_consent','mcn_account_new')`, id).Scan(&audits))
	if audits != 5 {
		t.Fatalf("MCN writes recorded %d audit rows, want 5", audits)
	}
	closeBody := fmt.Sprintf(`{"id":%q}`, streamer)
	request(mcn, "POST", route+"no_renewal", two, closeBody, 200, "4005")
	request(mcn, "POST", route+"no_renewal", manager, closeBody, 403, "403")
	room, err := a.Rooms.Start(ctx, creator.User.ID, "Contract room", "")
	check(err)
	check(a.Rooms.Join(ctx, room.ID, viewer.User.ID, ""))
	buy := fmt.Sprintf(`{"date_room_id":"%d","live_id":"%d","gift_id":"87","sender_id":"%d","receiver_id":"%d","quantity":1}`,
		room.ID, room.Session.LiveID, viewer.User.ID, creator.User.ID)
	request(public, "POST", "/api/date/1vN/give/gifts", viewer.Token, buy, 200, "200")
	_, err = db.Exec(ctx, `INSERT INTO mcn_legacy_settlement_proofs(settlement_id,agency_id,streamer_id,gross_ld,bad_debt_ld,
		provider_verified,source_pending,paid_at) VALUES(192,21,$1,$2,3,FALSE,TRUE,clock_timestamp())`,
		creator.User.ID, int64(9007199254740999))
	check(err)
	request(mcn, "POST", route+"no_renewal", one, closeBody, 409, "409")
	check(db.QueryRow(ctx, `SELECT agency_id FROM anchor_profiles WHERE user_id=$1`, creator.User.ID).Scan(&agency))
	if agency != 21 {
		t.Fatalf("uncertain debt prematurely detached agency: %d", agency)
	}
	_, err = db.Exec(ctx, `UPDATE mcn_legacy_settlement_proofs SET provider_verified=TRUE,provider_order_id=9007199254741202,
		provider_order_no='old-settled-order',charged=9007199254740999 WHERE settlement_id=192`)
	check(err)
	request(mcn, "POST", route+"no_renewal", one, closeBody, 200, "200")
	request(mcn, "POST", route+"no_renewal", one, closeBody, 200, "200")
	var trace, gross, debt int64
	check(db.QueryRow(ctx, `SELECT trace_id,gross_ld,bad_debt_ld FROM mcn_final_settlements WHERE membership_id=$1`, row).Scan(&trace, &gross, &debt))
	if trace <= 0 || gross != 9007199254741007 || debt != 7 {
		t.Fatalf("wrong final settled points %d/%d (trace %d)", gross, debt, trace)
	}
	var proofRows, traces int
	check(db.QueryRow(ctx, `SELECT count(*) FROM mcn_final_settlement_items WHERE trace_id=$1 AND provider_order_id IN
		(9007199254741201,9007199254741202)`, trace).Scan(&proofRows))
	check(db.QueryRow(ctx, `SELECT count(*) FROM mcn_final_settlements WHERE membership_id=$1`, row).Scan(&traces))
	if traces != 1 || proofRows != 2 {
		t.Fatalf("contract replay made %d traces, %d proofs", traces, proofRows)
	}
	check(db.QueryRow(ctx, `SELECT a.agency_id,m.status FROM anchor_profiles a JOIN backoffice_mcn_streamers m ON m.streamer_id=a.user_id
		WHERE m.id=$1`, row).Scan(&agency, &status))
	if agency != 0 || status != 7 {
		t.Fatalf("confirmed MCN termination = %d/%d", agency, status)
	}
}

type testMCNMailer struct{ sent []string }

func (m *testMCNMailer) SendPlainText(_ context.Context, address, subject, body string) error {
	if address != "creator@example.test" || subject != "LiveMedia agency invitation" {
		return fmt.Errorf("unexpected message")
	}
	m.sent = append(m.sent, body)
	return nil
}
