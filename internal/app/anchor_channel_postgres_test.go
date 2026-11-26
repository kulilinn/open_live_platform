package app_test

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"open_live_platform/internal/anchor"
	"open_live_platform/internal/app"
	"open_live_platform/internal/store/postgres"
)

func TestPostgresAnchorChannelReports(t *testing.T) {
	config, _ := postgresTestDatabase(t)
	ctx := t.Context()
	key, expired := strings.Repeat("testActive", 4), strings.Repeat("testExpired", 4)
	now := time.Now().UTC()
	config.AnchorChannelAccess = filepath.Join(t.TempDir(), "channel.json")
	content := fmt.Sprintf(`[{"channel":"primary","keyHash":"%x","expiresAt":%q},
		{"channel":"primary","keyHash":"%x","expiresAt":%q}]`, sha256.Sum256([]byte(key)), now.Add(time.Hour).Format(time.RFC3339),
		sha256.Sum256([]byte(expired)), now.Add(-time.Hour).Format(time.RFC3339))
	if err := os.WriteFile(config.AnchorChannelAccess, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	platform, err := app.New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(platform.Close)
	store, err := postgres.Open(ctx, config.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Shutdown)
	request := func(method, path, auth, body string, status int) string {
		t.Helper()
		r := httptest.NewRequestWithContext(ctx, method, "/anchor/api/"+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+auth)
		w := httptest.NewRecorder()
		platform.Server.Handler.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s = %d %s", path, w.Code, w.Body.String())
		}
		var reply struct{ Data json.RawMessage }
		if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		return string(reply.Data)
	}
	request("GET", "stat/total", expired, "", 401)
	user, err := platform.Accounts.SignUp(ctx, "channel@example.test", "password", "Channel")
	if err != nil {
		t.Fatal(err)
	}
	request("GET", "stat/total", user.Token, "", 401)
	request("GET", "stat/total", key, "", 503)
	day := time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC)
	old := day.AddDate(0, -1, 0)
	next := day.AddDate(0, 0, 1)
	users := []anchor.ChannelUser{{ID: 1, Channel: "primary", Joined: &day},
		{ID: 2, Channel: "primary", Joined: &old}, {ID: 3, Channel: "other", Joined: &day}}
	spends := []anchor.ChannelSpend{{ID: "primary-a", UserID: 1, Points: 50, Created: day},
		{ID: "primary-b", UserID: 1, Points: 30, Created: day.Add(4 * time.Hour)},
		{ID: "primary-c", UserID: 2, Points: 20, Created: day.Add(12 * time.Hour)},
		{ID: "other-a", UserID: 3, Points: 40, Created: day},
		{ID: "outside", UserID: 2, Points: 9007199254740993, Created: next}}
	rates := []anchor.ChannelRate{{Channel: "primary", Ratio: 2500}, {Channel: "other", Ratio: 5000}}
	load := func(u []anchor.ChannelUser, s []anchor.ChannelSpend) error {
		return store.ImportLegacyChannels(ctx, channelRows(u), channelRows(s), channelRows(rates))
	}
	if err := load(users, spends); err != nil {
		t.Fatal(err)
	}
	if err := load(users, spends); err != nil {
		t.Fatal(err)
	}
	if data := request("GET", "stat/day?search_date=2025-05-01&channel=other", key, "", 200); data != `{"buyer_num":2,"channel":"primary","register_num":1}` {
		t.Fatalf("daily channel report = %s", data)
	}
	if data := request("GET", "stat/total", key, "", 200); data != `{"buyer_num":2,"channel":"primary","register_num":2}` {
		t.Fatalf("total channel report = %s", data)
	}
	if data := request("POST", "game/income", key, `{"date":"2025-05-01"}`, 200); data != `{"buy_users":2,"income":"10.00","into":"2.50","number":3,"users":2}` {
		t.Fatalf("game daily report = %s", data)
	}
	if data := request("POST", "game/income", key, `{"date":"2025-05"}`, 200); !strings.Contains(data, `"income":"900719925474109.30"`) ||
		!strings.Contains(data, `"into":"225179981368527.33"`) {
		t.Fatalf("precision of game monthly report = %s", data)
	}
	for _, path := range []string{"hardware/income", "hardware/settlement"} {
		if data := request("POST", path, key, `{"date":"2025-05"}`, 200); data != `{"income":0,"into":0,"number":0}` {
			t.Fatalf("legacy hardware response = %s", data)
		}
	}
	fail := errors.New("test interrupted legacy data read")
	err = store.ImportLegacyChannels(ctx, channelRows(users[:1]), func(yield func(anchor.ChannelSpend, error) bool) { yield(anchor.ChannelSpend{}, fail) }, channelRows(rates))
	if !errors.Is(err, fail) {
		t.Fatalf("interrupted snapshot error = %v", err)
	}
	if data := request("GET", "stat/total", key, "", 200); !strings.Contains(data, `"register_num":2`) {
		t.Fatalf("partial import replaced prior snapshot: %s", data)
	}
	if err := load(users[:1], spends[:1]); err != nil {
		t.Fatal(err)
	}
	if data := request("GET", "stat/total", key, "", 200); !strings.Contains(data, `"register_num":1`) {
		t.Fatalf("current snapshot still contains stale users: %s", data)
	}
}

func channelRows[T any](values []T) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		for _, value := range values {
			if !yield(value, nil) {
				return
			}
		}
	}
}
