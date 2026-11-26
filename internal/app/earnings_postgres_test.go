package app_test

import (
	"encoding/json"
	"fmt"
	"iter"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"open_live_platform/internal/app"
	"open_live_platform/internal/gift"
	"open_live_platform/internal/live"
	"open_live_platform/internal/store/postgres"
)

func TestPostgresImportedEarnings(t *testing.T) {
	config, _ := postgresTestDatabase(t)
	config.EdgeToken = "earnings-test-edge"
	ctx := t.Context()
	platform, err := app.New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(platform.Close)
	owner, err := platform.Accounts.SignUp(ctx, "earnings@example.test", "password", "Earnings")
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := platform.Accounts.SignUp(ctx, "earning-giver@example.test", "password", "Viewer")
	if err != nil {
		t.Fatal(err)
	}
	anchorID := owner.User.ID
	_, err = platform.Rooms.Start(ctx, anchorID, "Streaming", "")
	if err != nil {
		t.Fatal(err)
	}
	store, err := postgres.Open(ctx, config.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Shutdown)
	now := time.Now().UTC()
	lines := func(yield func(gift.EarningsLine, error) bool) {
		for _, line := range []gift.EarningsLine{
			{ID: 9007199254740998, AnchorID: anchorID, GiverID: viewer.User.ID, Type: 6, Points: 7, CreatedAt: now.Add(-2 * time.Hour), Description: "Legacy sub"},
			{ID: 9007199254740999, AnchorID: anchorID, GiverID: viewer.User.ID, Type: 7, Points: 9, CreatedAt: now.AddDate(0, 0, -2), Description: "Legacy renewal"},
			{ID: 9007199254741000, AnchorID: anchorID, GiverID: viewer.User.ID, Type: 2, Points: 11, CreatedAt: now.Add(-time.Hour), Description: "Legacy gift"},
			{ID: 9007199254741001, AnchorID: anchorID, GiverID: viewer.User.ID, Type: 2, Points: 13, CreatedAt: now.AddDate(-1, 0, 0), Description: "Earlier legacy gift"},
		} {
			if !yield(line, nil) {
				return
			}
		}
	}
	count, err := store.ImportLegacySettlements(ctx, iter.Seq2[gift.EarningsLine, error](lines))
	if count != 4 || err != nil {
		t.Fatalf("first legacy import = %d err = %v", count, err)
	}
	count, err = store.ImportLegacySettlements(ctx, iter.Seq2[gift.EarningsLine, error](lines))
	if count != 0 || err != nil {
		t.Fatalf("repeat legacy import = %d err = %v", count, err)
	}
	request := func(path, token, service string, wantCode int) json.RawMessage {
		t.Helper()
		r := httptest.NewRequestWithContext(ctx, "GET", path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Inner-Service", service)
		w := httptest.NewRecorder()
		platform.Server.Handler.ServeHTTP(w, r)
		var out struct {
			Data json.RawMessage
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); w.Code != wantCode || err != nil {
			t.Fatalf("%s = %d %s, err=%v", path, w.Code, w.Body.String(), err)
		}
		return out.Data
	}
	activePath := "/api/earning/living_anchor_list"
	request(activePath, owner.Token, "admin", 403)
	request(activePath, config.EdgeToken, "", 403)
	living := request(activePath, config.EdgeToken, "admin", 200)
	var livingIDs []int64
	if err := json.Unmarshal(living, &livingIDs); err != nil || !slices.Equal(livingIDs, []int64{anchorID}) {
		t.Fatalf("active anchors = %s, error = %v", living, err)
	}
	for _, status := range []live.RoomStatus{live.RoomLiveShow, live.RoomAnchorAway, live.RoomOffline} {
		if _, err := platform.Rooms.SetRoomStatus(ctx, anchorID, anchorID, status); err != nil {
			t.Fatal(err)
		}
		living = request(activePath, config.EdgeToken, "innow", 200)
		var want []int64
		if status != live.RoomOffline {
			want = []int64{anchorID}
		}
		if err := json.Unmarshal(living, &livingIDs); err != nil || !slices.Equal(livingIDs, want) {
			t.Fatalf("active anchors with status %d = %s, error = %v", status, living, err)
		}
	}
	raw := request("/anchor/api/stat_income_sometime?date_type=3&income_type=6", owner.Token, "", 200)
	var summary struct {
		List []gift.EarningsBucket
		Sum  int64
	}
	if err := json.Unmarshal(raw, &summary); err != nil || summary.Sum != 16 ||
		len(summary.List) < 1 || len(summary.List[0].Date) != 7 {
		t.Fatalf("monthly statement = %s err = %v", raw, err)
	}
	raw = request("/anchor/api/get_income_details?date_type=1&income_type=7&page=1&page_size=1", owner.Token, "", 200)
	var details struct {
		List  []gift.EarningsLine
		Total int64
	}
	if err := json.Unmarshal(raw, &details); err != nil || details.Total != 2 || len(details.List) != 1 ||
		details.List[0].ID != 9007199254740998 || details.List[0].GiverName != "Viewer" || details.List[0].Description != "Legacy sub" {
		t.Fatalf("settlement page = %s err = %v", raw, err)
	}
	raw = request("/anchor/api/get_income_details?date_type=1&income_type=7&page=3&page_size=1", owner.Token, "", 200)
	if !strings.Contains(string(raw), `"current":3`) || !strings.Contains(string(raw), `"list":[]`) {
		t.Fatalf("out of range page = %s", raw)
	}
	request(fmt.Sprintf("/anchor/api/stat_fans_income?anchor_id=%d", anchorID), viewer.Token, "", 403)
	raw = request("/anchor/api/stat_fans_income", owner.Token, "", 200)
	if !strings.Contains(string(raw), `"income_total":40`) {
		t.Fatalf("all-time income dashboard = %s", raw)
	}
}
