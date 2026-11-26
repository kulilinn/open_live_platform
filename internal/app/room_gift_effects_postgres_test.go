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
	"open_live_platform/internal/auth"
	"open_live_platform/internal/store/postgres"
)

func TestPostgresWheelHardwareAndRocketAcrossReplicas(t *testing.T) {
	cfg, db := postgresTestDatabase(t)
	ctx := t.Context()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var debited atomic.Int64
	var calls atomic.Int64
	billing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/payment/point/decrease" {
			http.NotFound(w, r)
			return
		}
		var input struct{ Point int64 }
		if json.NewDecoder(r.Body).Decode(&input) != nil || input.Point <= 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		id := calls.Add(1)
		debited.Add(input.Point)
		fmt.Fprintf(w, `{"code":200,"data":{"order_id":"%d","order_no":"gift-%d","total_point":%d}}`, id, id, input.Point)
	}))
	t.Cleanup(billing.Close)
	cfg.PaymentHost, cfg.PaymentAppID = billing.URL, "wheel-test"
	cfg.RocketLikeSpeedCoefficient = 0.5
	cfg.GiftCatalogFile = filepath.Join(t.TempDir(), "gifts.json")
	check(os.WriteFile(cfg.GiftCatalogFile, []byte(`[
		{"id":"901","name":"Toy","price":20,"giftType":2,"effectControlType":2,"parameterOne":"20","parameterTwo":15},
		{"id":"902","name":"Rocket","price":200,"effectControlType":6,"parameterOne":"30","parameterTwo":20}
	]`), 0600))
	open := func() *app.App {
		p, err := app.New(ctx, cfg)
		check(err)
		t.Cleanup(p.Close)
		return p
	}
	first, peer := open(), open()
	signUp := func(name string) auth.Session {
		v, err := first.Accounts.SignUp(ctx, name+"@effects.test", "password", name)
		check(err)
		return v
	}
	anchor, viewer, stranger := signUp("anchor"), signUp("viewer"), signUp("stranger")
	room, err := first.Rooms.Start(ctx, anchor.User.ID, "Live", "")
	check(err)
	check(peer.Rooms.Join(ctx, room.ID, viewer.User.ID, ""))
	id := strconv.FormatInt(room.ID, 10)
	request := func(a *app.App, who auth.Session, method, path, key, body string, code int, status string) json.RawMessage {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+who.Token)
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		w := httptest.NewRecorder()
		a.Server.Handler.ServeHTTP(w, r)
		var out struct {
			Status string
			Data   json.RawMessage
		}
		check(json.NewDecoder(w.Body).Decode(&out))
		if w.Code != code || (status != "" && out.Status != status) {
			t.Fatalf("%s %s = %d/%s (%s) want %d/%s", method, path, w.Code, out.Status, w.Body.String(), code, status)
		}
		return out.Data
	}
	subscription, err := peer.Rooms.SubscribeWithTranslations(ctx, room.ID, anchor.User.ID)
	check(err)
	defer subscription.Unsubscribe()
	events := subscription.Events
	params := fmt.Sprintf(`{"anchor_id":%q,"price":17,"value":["Spin"],"open_status":0}`, id)
	request(first, stranger, "POST", "/api/date/set/wheel/price", "", params, 403, "")
	request(first, anchor, "POST", "/api/date/set/wheel/price", "", params, 200, "200")
	request(peer, anchor, "POST", "/api/date/set/wheel/value", "", params, 200, "200")
	request(first, anchor, "POST", "/api/date/wheel/open_or_close", "", params, 200, "200")
	play := fmt.Sprintf(`{"anchorId":%q,"userId":"%d","chatRoomId":%q,"dateRoomId":%q,"liveId":"%d"}`, id, viewer.User.ID, id, id, room.Session.LiveID)
	request(first, stranger, "POST", "/api/date/play_wheel", "bad", play, 403, "")
	request(first, viewer, "POST", "/api/date/play_wheel", "wheel", play, 200, "200")
	request(peer, viewer, "POST", "/api/date/play_wheel", "wheel", play, 200, "200")
	for _, kind := range []int{12, 13, 14} {
		select {
		case event := <-events:
			var payload struct {
				Message struct {
					Type int `json:"msgType"`
				}
			}
			check(json.Unmarshal([]byte(event.Payload), &payload))
			if event.Type != "gift" || payload.Message.Type != kind {
				t.Fatalf("wheel frame = %s, want type %d", event.Payload, kind)
			}
		case <-time.After(4 * time.Second):
			t.Fatalf("wheel frame %d timeout", kind)
		}
	}
	results := request(peer, anchor, "GET", "/api/date/get/wheel/result?dateRoomId="+id+"&page=1&pageSize=10", "", "", 200, "200")
	if !strings.Contains(string(results), `"result_value":"Spin"`) || debited.Load() != 17 || calls.Load() != 1 {
		t.Fatalf("wheel paid %d times for %d, results %s", calls.Load(), debited.Load(), results)
	}
	request(peer, anchor, "POST", "/api/date/gift/hardware/set", "", fmt.Sprintf(`{"dateRoomId":%q,"reduceTimeRatio":25}`, id), 200, "200")
	give := func(a *app.App, item int, path, key, status string, code int) {
		send := fmt.Sprintf(`{"date_room_id":%q,"live_id":"%d","gift_id":"%d","limit_id":"0","sender_id":"%d","receiver_id":%q,"quantity":1}`,
			id, room.Session.LiveID, item, viewer.User.ID, id)
		request(a, viewer, "POST", path, key, send, code, status)
	}
	give(first, 901, "/api/date/1vN/give/gifts", "toy", "200", 200)
	check(peer.Rooms.(*postgres.Store).SweepRoomGiftEffects(ctx))
	check(first.Rooms.(*postgres.Store).SweepRoomGiftEffects(ctx))
	kind8 := false
	timeout := time.After(4 * time.Second)
	for !kind8 {
		select {
		case event := <-events:
			var payload struct {
				Message struct {
					Type int `json:"msgType"`
				}
			}
			check(json.Unmarshal([]byte(event.Payload), &payload))
			kind8 = event.Type == "gift" && payload.Message.Type == 8
		case <-timeout:
			t.Fatal("queued hardware gift start frame timeout")
		}
	}
	queue := request(peer, anchor, "GET", "/api/date/hardware/queue/get/"+id, "", "", 200, "200")
	if !strings.Contains(string(queue), `"operation_time":15`) {
		t.Fatalf("duration reduction not reflected in queue: %s", queue)
	}
	give(peer, 902, "/api/date/1vN/give/rocket", "rocket", "200", 200)
	give(first, 902, "/api/date/1vN/give/rocket", "rocket-again", "409", 409)
	state := request(first, viewer, "GET", "/api/date/rocket/state/get/"+id, "", "", 200, "200")
	if !strings.Contains(string(state), `"vibration_intensity":20`) {
		t.Fatalf("rocket launch state = %s", state)
	}
	speed := fmt.Sprintf(`{"room_id":%q,"date_room_id":%q,"anchor_id":%q,"user_id":"%d","likes_count":100}`, id, id, id, viewer.User.ID)
	request(peer, viewer, "POST", "/api/date/rocket/change/speed", "", speed, 200, "200")
	state = request(first, viewer, "GET", "/api/date/rocket/state/get/"+id, "", "", 200, "200")
	if !strings.Contains(string(state), `"vibration_intensity":30`) {
		t.Fatalf("rocket speed after likes = %s", state)
	}
	_, err = db.Exec(ctx, `INSERT INTO room_rocket_contributions(order_id,user_id,score)
		SELECT order_id,$1,23 FROM room_gift_effects WHERE effect=6`, viewer.User.ID)
	check(err)
	rank := request(peer, viewer, "GET", "/api/1vN/rocket/refuel/rank/"+id, "", "", 200, "200")
	if !strings.Contains(string(rank), `"scores":"23"`) || debited.Load() != 237 || calls.Load() != 3 {
		t.Fatalf("rocket rank %s and debits %d/%d", rank, calls.Load(), debited.Load())
	}
	check(first.Rooms.Close(ctx, room.ID, anchor.User.ID))
	result := request(peer, viewer, "GET", "/api/date/get/rocket/final/result?dateRoomId="+id+"&anchorId="+id, "", "", 200, "200")
	if !strings.Contains(string(result), `"rocket_is_end":true`) {
		t.Fatalf("rocket final view after close=%s", result)
	}
}
