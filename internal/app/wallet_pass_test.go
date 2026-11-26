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
	"time"

	"open_live_platform/internal/app"
	"open_live_platform/internal/upload"
)

func TestWalletPassDownload(t *testing.T) {
	const key = "9007199254741091"
	const expired = "9007199254741092"
	const content = "already signed pass bytes"
	var downloads atomic.Int64
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/passes-test/prefix/passes/417/"+key+".pkpass" ||
			!strings.Contains(r.Header.Get("Authorization"), "Credential=fixture-key/") || r.Header.Get("X-Amz-Content-Sha256") == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		fmt.Fprint(w, content)
	}))
	t.Cleanup(s3.Close)
	index := filepath.Join(t.TempDir(), "passes.jsonl")
	var records strings.Builder
	for i, passKey := range []string{key, expired} {
		expires := time.Now().UTC().Add(time.Hour - time.Duration(i)*2*time.Hour)
		if err := json.NewEncoder(&records).Encode(map[string]any{"key": passKey + "|pkpass|source-app", "object": "/passes/417/" + passKey + ".pkpass", "expires_at": expires}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(index, []byte(records.String()), 0600); err != nil {
		t.Fatal(err)
	}
	platform, err := app.New(t.Context(), app.Config{WalletPasses: upload.PassConfig{IndexFile: index, AppName: "source-app", Storage: upload.Config{
		Endpoint: s3.URL, Bucket: "passes-test", Parent: "/prefix", KeyID: "fixture-key", Secret: "fixture-secret",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(platform.Close)
	read := func(key string) *httptest.ResponseRecorder {
		t.Helper()
		result := httptest.NewRecorder()
		platform.Server.Handler.ServeHTTP(result, httptest.NewRequest(http.MethodGet, "/pass/"+key, nil))
		return result
	}
	result := read(key)
	if result.Code != http.StatusOK || result.Body.String() != content || result.Header().Get("Content-Type") != "application/vnd.apple.pkpass" ||
		result.Header().Get("Content-Disposition") != "attachment; filename="+key+".pkpass" {
		t.Fatalf("wallet pass = %d %s %v", result.Code, result.Body.String(), result.Header())
	}
	for _, key := range []string{expired, "invalid", "9007199254741093"} {
		result := read(key)
		var body struct{ Status string }
		if err := json.Unmarshal(result.Body.Bytes(), &body); err != nil || result.Code != http.StatusOK || body.Status != "4005" {
			t.Fatalf("expired or missing wallet pass = %d %s", result.Code, result.Body.String())
		}
	}
	if downloads.Load() != 1 {
		t.Fatalf("invalid wallet pass requested S3: %d downloads", downloads.Load())
	}
}
