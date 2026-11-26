package app_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"open_live_platform/internal/admin"
	"open_live_platform/internal/live"
	"open_live_platform/internal/store/postgres"
	"strconv"
	"testing"
	"time"
)

func TestImportLegacyRoomAndLedgerHistory(t *testing.T) {
	cfg, db := postgresTestDatabase(t)
	ctx := t.Context()
	store, err := postgres.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Shutdown()
	u, err := store.SignUp(ctx, "roomhistory@example.test", "pass", "previous owner")
	if err != nil {
		t.Fatal(err)
	}
	id := u.User.ID
	uid := strconv.FormatInt(id, 10)
	const broadcast int64 = 79123456789012345
	record := func(kind, key, data string) postgres.HistoryRow {
		return postgres.HistoryRow{Source: kind, ID: key, Owner: id, Record: json.RawMessage(data)}
	}
	room := record("room", uid, `{"subject":"archived","create_at":"2021-10-10 13:10:00","banner":"cover.jpg","tag":"tag","description":"memo","offline_info":"offline","plan":"[]","gifts":"[]","trans":1,"trans_lang":"ja","limited":1,"online":1}`)
	room.Password = "legacy-password"
	metric := record("room_metrics", strconv.FormatInt(broadcast, 10), fmt.Sprintf(`{"room":"%d","start_time":"2021-10-11 13:10:00","end_time":"2021-10-11 13:15:00","audience":11,"income":54,"new_fans":2,"gift_fans":3,"like_quantity":7}`, id))
	balance := record("user_points", uid, `{"point":40000000001234567}`)
	stars := record("star_coin_flow_records", "91811189012345679", `{"balance":231,"create_time":"2021-10-11 13:10:00"}`)
	add := func(rows ...postgres.HistoryRow) postgres.HistoryCounts {
		count, err := store.ImportLegacyHistory(ctx, legacyRows(rows...))
		if err != nil {
			t.Fatal(err)
		}
		return count
	}
	if got := add(room, metric, balance, stars); got.Rooms != 1 || got.Broadcasts != 1 || got.Changed != 4 || got.Stars != 1 {
		t.Fatal(got)
	}
	if got := add(room, metric, balance, stars); got.Read != 4 || got.Changed != 0 || got.Rooms != 0 || got.Stars != 0 {
		t.Fatal(got)
	}
	r, err := store.Info(ctx, id)
	if err != nil || r.Online || r.MemberCount != 0 || !r.IsLock || r.Title != "archived" || !r.Limited(time.Now()) {
		t.Fatalf("room %+v %v", r, err)
	}
	set, err := store.RoomSettlement(ctx, broadcast, id)
	if err != nil || set.Time != 300 || set.Income != 54 || set.FanCount != 11 || set.NewFanCount != 2 || set.GiftUserCount != 3 {
		t.Fatalf("settlement %+v %v", set, err)
	}
	if _, err := store.RoomSettlement(ctx, broadcast, id+1); !errors.Is(err, live.ErrForbidden) {
		t.Fatal("incorrect owner", err)
	}
	var points int64
	if err := db.QueryRow(ctx, `SELECT (record->>'point')::bigint FROM legacy_business_history WHERE source_table='user_points' AND source_id=$1`, uid).Scan(&points); err != nil || points != 40000000001234567 {
		t.Fatal(points, err)
	}
	if _, err := db.Exec(ctx, "UPDATE rooms SET title='updated' WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	add(room)
	r, err = store.Info(ctx, id)
	if err != nil || r.Title != "updated" {
		t.Fatal("import overwrote current room", err)
	}
	application := record("anchor_users", uid, `{"state":1,"channel":"historical","country":"JP","kyc_status":1}`)
	identity := record("anchor_users_kyc", uid, `{"applicant_id":"","name":"Fixture","ja_first_name":"Example","status":2,"create_time":"2021-10-10 13:00:00"}`)
	if counts := add(application, identity); counts.Changed != 2 {
		t.Fatal(counts)
	}
	data, err := store.AnchorReviewDetail(ctx, id, true)
	var imported struct {
		JapaneseName string `json:"ja_first_name"`
	}
	if err != nil || json.Unmarshal(data, &imported) != nil || imported.JapaneseName != "Example" {
		t.Fatalf("imported identity %s, err=%v", data, err)
	}
	if counts := add(application, identity); counts.Changed != 0 {
		t.Fatal(counts)
	}
	if err := store.CancelAnchorReview(ctx, admin.Principal{ID: 7, Area: admin.OperationArea, Tenant: admin.OperationTenant, Username: "reviewer"}, "/cancel", id); err != nil {
		t.Fatal(err)
	}
	if counts := add(application, identity); counts.Changed != 0 {
		t.Fatalf("cancelled anchor restored by import: %+v", counts)
	}
	if _, err := store.AnchorReviewDetail(ctx, id, true); !errors.Is(err, admin.ErrBackofficeNotFound) {
		t.Fatalf("cancelled identity still present: %v", err)
	}
	broken := room
	broken.Record = json.RawMessage(`{"plan":"broken"}`)
	changed := balance
	changed.Record = json.RawMessage(`{"point":1}`)
	if _, err := store.ImportLegacyHistory(ctx, legacyRows(changed, broken)); err == nil {
		t.Fatal("invalid archive accepted")
	}
	var same bool
	if err := db.QueryRow(ctx, `SELECT record=$1::jsonb FROM legacy_business_history WHERE source_table='user_points' AND source_id=$2`, balance.Record, uid).Scan(&same); err != nil || !same {
		t.Fatal("partial write", err)
	}
}
