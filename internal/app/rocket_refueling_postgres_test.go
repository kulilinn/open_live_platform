package app_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"open_live_platform/internal/app"
	"open_live_platform/internal/gift"
	"open_live_platform/internal/relay"
	"open_live_platform/internal/store/postgres"
)

func TestPostgresRocketRefuelSettlesWeightedRewardsAndPool(t *testing.T) {
	cfg, db := postgresTestDatabase(t)
	ctx := t.Context()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int64
	billing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/payment/point/decrease" || r.Header.Get("APPID") != "rocket-test" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Points int64  `json:"point"`
			User   int64  `json:"user_id"`
			Order  string `json:"order_no"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || r.Header.Get("USER_ID") != strconv.FormatInt(body.User, 10) || !strings.HasPrefix(body.Order, "gift-") {
			w.WriteHeader(400)
			return
		}
		id := calls.Add(1)
		switch body.Points {
		case 30:
			fmt.Fprint(w, `{"code":400,"reason":"POINT_NOT_ENOUGH"}`)
		case 40:
			w.WriteHeader(502)
		default:
			fmt.Fprintf(w, `{"code":200,"data":{"order_id":"%d","order_no":"rocket-%d","total_point":%d}}`, id, id, body.Points)
		}
	}))
	t.Cleanup(billing.Close)
	cfg.PaymentHost, cfg.PaymentAppID = billing.URL, "rocket-test"
	cfg.RelayRust = relay.RustOptions{AppKey: "rocket-test", AppSecret: "rocket-test-secret"}
	dir := t.TempDir()
	cfg.GiftCatalogFile = filepath.Join(dir, "gifts.json")
	check(os.WriteFile(cfg.GiftCatalogFile, []byte(`[
		{"id":"100","name":"Rocket","price":200,"effectControlType":6,"parameterOne":"30","parameterTwo":20},
		{"id":"101","name":"Fuel","price":10,"effectControlType":7},
		{"id":"102","name":"Award","price":0,"incomeStream":25}
	]`), 0600))
	rules := filepath.Join(dir, "rocket.json")
	check(os.WriteFile(rules, []byte(`{
		"initial_jackpot":30,"platform_income_ratio":0.25,
		"rewards":[
			{"id":"1","gift_id":"102","pool_condition":0,"weight":1,"reward_value":21,"reward_multiplier":3,"platform_exp":6,"fuel_points":2},
			{"id":"2","gift_id":"102","pool_condition":100000,"weight":500,"reward_value":55,"reward_multiplier":9,"platform_exp":19,"fuel_points":9}
		],
		"time_rules":[{"time_condition":30,"time_coefficient":10},{"time_condition":1000,"time_coefficient":500}],
		"notices":[
			{"notice_type":1,"config_condition":3,"content":"equal threshold is skipped"},
			{"notice_type":1,"config_condition":4,"content":"deleted","del_flag":1},
			{"notice_type":1,"config_condition":5,"content":"Critical"},
			{"notice_type":2,"config_condition":2,"live_room_flag":0,"broadcast_config_id":"40"},
			{"notice_type":2,"config_condition":3,"live_room_flag":1}
		]
	}`), 0600))
	open := func(config app.Config) *app.App {
		p, err := app.New(ctx, config)
		check(err)
		t.Cleanup(p.Close)
		return p
	}
	unconfigured := open(cfg)
	cfg.RocketConfigFile = rules
	first, second := open(cfg), open(cfg)
	_, err := db.Exec(ctx, `INSERT INTO public_catalog(section,records) VALUES('broadcast_configs',
		'[{"id":40,"jump_type":1,"jump_target":"test-room","jump_background":"plain","jump_name":"Go","background":"blue"}]'::jsonb)`)
	check(err)
	owner, err := first.Accounts.SignUp(ctx, "owner@rocket.test", "password", "Owner")
	check(err)
	viewer, err := first.Accounts.SignUp(ctx, "viewer@rocket.test", "password", "Viewer")
	check(err)
	outside, err := first.Accounts.SignUp(ctx, "outside@rocket.test", "password", "Outside")
	check(err)
	room, err := first.Rooms.Start(ctx, owner.User.ID, "Rocket Live", "")
	check(err)
	check(second.Rooms.Join(ctx, room.ID, viewer.User.ID, ""))
	send := func(p *app.App, item, quantity int, key string, code int, status string) {
		t.Helper()
		stream := fmt.Sprintf(`{"date_room_id":"%d","live_id":"%d","gift_id":"%d","limit_id":"0","sender_id":"%d","receiver_id":"%d","quantity":%d}`,
			room.ID, room.Session.LiveID, item, viewer.User.ID, owner.User.ID, quantity)
		req := httptest.NewRequest(http.MethodPost, "/api/date/1vN/give/gifts", strings.NewReader(stream)).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer "+viewer.Token)
		req.Header.Set("Idempotency-Key", key)
		out := httptest.NewRecorder()
		p.Server.Handler.ServeHTTP(out, req)
		var result struct{ Status string }
		check(json.NewDecoder(out.Body).Decode(&result))
		if out.Code != code || result.Status != status {
			t.Fatalf("order %s got %d/%s (%s), want %d/%s", key, out.Code, result.Status, out.Body.String(), code, status)
		}
	}
	send(first, 101, 1, "too-early", 200, "4086")
	send(first, 100, 1, "launch", 200, "200")
	send(unconfigured, 101, 1, "unconfigured", 503, "503")
	if calls.Load() != 1 {
		t.Fatalf("invalid attempts billed %d times", calls.Load())
	}
	sub, err := first.Rooms.SubscribeWithTranslations(ctx, room.ID, owner.User.ID)
	check(err)
	defer sub.Unsubscribe()
	server := httptest.NewServer(first.Server.Handler)
	t.Cleanup(server.Close)
	socket := openPrivateH5(t, server, outside)
	t.Cleanup(func() { socket.Close() })
	send(second, 101, 2, "fuel", 200, "200")
	send(first, 101, 2, "fuel", 200, "200")
	for {
		var broadcast struct {
			Content, Queue string
			Payload        struct {
				Type int `json:"broadcast_type"`
				Data struct {
					Combo int    `json:"combo"`
					Jump  string `json:"jump_target"`
				} `json:"data"`
			}
		}
		_, wire, err := socket.ReadMessage()
		check(err)
		var envelope struct{ Content string }
		check(json.Unmarshal(wire, &envelope))
		if envelope.Content != "notification" {
			continue
		}
		check(json.Unmarshal(wire, &broadcast))
		if broadcast.Queue != strconv.FormatInt(outside.User.ID, 10) || broadcast.Payload.Type != 6 ||
			broadcast.Payload.Data.Combo != 2 || broadcast.Payload.Data.Jump != "test-room" {
			t.Fatalf("unexpected rocket broadcast %s", wire)
		}
		break
	}
	notices := make(map[string]bool)
	var criticalFrames, comboFrames int
	deadline := time.After(3 * time.Second)
	for criticalFrames < 2 || comboFrames < 1 {
		select {
		case event, open := <-sub.Events:
			if !open {
				t.Fatal("rocket room subscription closed")
			}
			if event.Type != "gift" {
				continue
			}
			var frame struct {
				ID, Channel string
				Message     struct {
					Type int             `json:"msgType"`
					Data json.RawMessage `json:"message"`
				}
			}
			check(json.Unmarshal([]byte(event.Payload), &frame))
			if frame.Message.Type != 5 && frame.Message.Type != 18 {
				continue
			}
			var data struct {
				Strike int    `json:"critical_strike"`
				Combo  int    `json:"combo"`
				Text   string `json:"context"`
			}
			check(json.Unmarshal(frame.Message.Data, &data))
			switch frame.Message.Type {
			case 5:
				if frame.Channel != "realtime" || data.Strike != 5 || data.Text != "Critical" || notices[frame.ID] {
					t.Fatalf("critical frame %s", event.Payload)
				}
				notices[frame.ID] = true
				criticalFrames++
			case 18:
				if frame.Channel != "realtime" || data.Combo != 2 {
					t.Fatalf("combo frame %s", event.Payload)
				}
				comboFrames++
			}
		case <-deadline:
			t.Fatalf("rocket notices: critical=%d combo=%d", criticalFrames, comboFrames)
		}
	}
	send(second, 101, 3, "decline", 200, "4046")
	send(first, 101, 3, "decline", 200, "4046")
	store := first.Rooms.(*postgres.Store)
	platform, fan, err := store.SubscriptionExperience(ctx, viewer.User.ID, owner.User.ID)
	check(err)
	if platform != 232 || fan != 0 {
		t.Fatalf("rocket settlement experience: platform=%d fan=%d", platform, fan)
	}
	rocket, err := store.RoomRocket(ctx, room.ID, room.Session.LiveID)
	check(err)
	if rocket == nil || rocket.End.Sub(rocket.Start).Seconds() != 32 {
		t.Fatalf("refueled flight = %+v", rocket)
	}
	rank, err := second.Rooms.(*postgres.Store).RocketRank(ctx, rocket.Order.ID)
	check(err)
	if len(rank) != 1 || rank[0].User != viewer.User.ID || rank[0].Score != 42 {
		t.Fatalf("paid refuel rank = %+v", rank)
	}
	var balance, reserved, contributed, fee, drawn int64
	check(db.QueryRow(ctx, `SELECT p.balance,p.reserved,(SELECT sum(contribution) FROM rocket_pool_ledger),
		(SELECT sum(platform_points) FROM rocket_pool_ledger),(SELECT sum(drawn) FROM rocket_pool_ledger)
		FROM rocket_jackpot p`).Scan(&balance, &reserved, &contributed, &fee, &drawn))
	if balance != 152 || reserved != 0 || contributed != 164 || fee != 55 || drawn != 42 || calls.Load() != 3 {
		t.Fatalf("pool %d reserved %d contribution %d platform %d draws %d provider %d", balance, reserved, contributed, fee, drawn, calls.Load())
	}
	income, err := store.GiftIncome(ctx, room.ID, room.Session.LiveID)
	check(err)
	var awards int64
	for _, entry := range income {
		if entry.GiftName == "Award" {
			awards += entry.Points
		}
	}
	if awards != 50 {
		t.Fatalf("actual award earnings = %d %+v", awards, income)
	}
	send(second, 101, 4, "payment-unknown", 409, "409")
	send(first, 101, 4, "payment-unknown", 409, "409")
	check(db.QueryRow(ctx, `SELECT balance,reserved FROM rocket_jackpot`).Scan(&balance, &reserved))
	if balance != 152 || reserved != 56 || calls.Load() != 4 {
		t.Fatalf("uncertain payment corrupted pool or duplicated debit: %d/%d/%d", balance, reserved, calls.Load())
	}
	var paid, declined, waiting int
	check(db.QueryRow(ctx, `SELECT count(*) FILTER(WHERE state=$1),count(*) FILTER(WHERE state=$2),count(*) FILTER(WHERE state=$3)
		FROM gift_orders`, gift.Paid, gift.Declined, gift.Pending).Scan(&paid, &declined, &waiting))
	if paid != 2 || declined != 1 || waiting != 1 {
		t.Fatalf("order decisions %d/%d/%d", paid, declined, waiting)
	}
	_, err = db.Exec(ctx, `UPDATE room_gift_effects SET start_at=now()-interval '33 seconds',
		motion_at=now()-interval '33 seconds',end_at=now()-interval '1 seconds' WHERE order_id=$1`, rocket.Order.ID)
	check(err)
	check(second.Rooms.(*postgres.Store).SweepRoomGiftEffects(ctx))
	check(store.SweepRoomGiftEffects(ctx))
	finished, err := store.RoomRocket(ctx, room.ID, room.Session.LiveID)
	check(err)
	if finished == nil || !strings.Contains(string(finished.FinalResult), `"msgType": 7`) ||
		!strings.Contains(string(finished.FinalResult), `"flight_distance": 640`) ||
		!strings.Contains(string(finished.FinalResult), `"contribute": 42`) {
		t.Fatalf("stored final rocket settlement = %+v", finished)
	}
}
