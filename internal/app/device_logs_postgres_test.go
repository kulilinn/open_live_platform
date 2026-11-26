package app_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"open_live_platform/internal/app"
	"open_live_platform/internal/relay"
	"open_live_platform/internal/upload"
)

func TestPostgresDeviceLogsUseLiveTicketAndSignedS3(t *testing.T) {
	config, db := postgresTestDatabase(t)
	ctx := t.Context()
	config.RelayRust = relay.RustOptions{AppKey: "device-test", AppSecret: "test-app-secret"}
	const logData = "device diagnostics example\n"
	var writes atomic.Int32
	var expectedPath atomic.Value
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil || string(data) != logData || r.URL.Path != expectedPath.Load() || r.Method != http.MethodPut {
			t.Errorf("incorrect S3 device log request: path=%s method=%s, size=%d, error=%v", r.URL.Path, r.Method, len(data), err)
		}
		digest := sha256.Sum256(data)
		if r.Header.Get("X-Amz-Content-Sha256") != hex.EncodeToString(digest[:]) || !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=test-key/") {
			t.Error("device log PUT did not include SigV4 credentials and content hash")
		}
		writes.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(s3.Close)
	config.Upload = upload.Config{Bucket: "logs-test", Endpoint: s3.URL, Parent: "platform", KeyID: "test-key", Secret: "test-secret"}
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	start := func() *app.App {
		t.Helper()
		platform, err := app.New(ctx, config)
		check(err)
		t.Cleanup(platform.Close)
		return platform
	}
	first, second := start(), start()
	user, err := first.Accounts.SignUp(ctx, "logs@example.org", "password", "Owner")
	check(err)
	expectedPath.Store("/logs-test/platform/toylogs/device-test/" + strconv.FormatInt(user.User.ID, 10) + "/sample.log")
	issue := func() (string, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/device/qrcode", nil).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer "+user.Token)
		res := httptest.NewRecorder()
		first.Server.Handler.ServeHTTP(res, req)
		var body struct{ URL string }
		check(json.NewDecoder(res.Body).Decode(&body))
		parsed, err := url.Parse(body.URL)
		check(err)
		token, sig, found := strings.Cut(strings.TrimPrefix(parsed.Path, "/api/connector/"), "/")
		if res.Code != http.StatusOK || !found {
			t.Fatal("could not receive Rust relay ticket for device logs")
		}
		return token, sig
	}
	submit := func(fileID, token, sig, content string, want int) {
		t.Helper()
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		for name, value := range map[string]string{"file": fileID, "token": token, "sig": sig} {
			check(form.WriteField(name, value))
		}
		file, err := form.CreateFormFile("data", "device.txt")
		check(err)
		_, err = io.WriteString(file, content)
		check(err)
		check(form.Close())
		req := httptest.NewRequest(http.MethodPost, "/api/device/logs", &body).WithContext(ctx)
		req.Header.Set("Content-Type", form.FormDataContentType())
		res := httptest.NewRecorder()
		second.Server.Handler.ServeHTTP(res, req)
		if res.Code != want {
			t.Fatalf("device log HTTP %d, want %d: %s", res.Code, want, res.Body.String())
		}
	}
	token, sig := issue()
	submit("sample.log", token, sig+"0", logData, http.StatusForbidden)
	submit("../sample.log", token, sig, logData, http.StatusBadRequest)
	submit("sample.log", token, sig, strings.Repeat("a", (5<<20)+1), http.StatusRequestEntityTooLarge)
	submit("sample.log", token, sig, logData, http.StatusOK)
	_, err = db.Exec(ctx, `UPDATE relay_tickets SET expires_at=statement_timestamp()-interval '1 second' WHERE token=$1`, token)
	check(err)
	submit("sample.log", token, sig, logData, http.StatusForbidden)
	token, sig = issue()
	check(first.Accounts.Revoke(ctx, user.Token))
	submit("sample.log", token, sig, logData, http.StatusForbidden)
	if writes.Load() != 1 {
		t.Fatalf("S3 observed %d PUTs, want only the valid log", writes.Load())
	}
}
