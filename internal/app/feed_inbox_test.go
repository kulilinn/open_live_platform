package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/dm"
)

type feedAudit struct{}

func (feedAudit) ApproveDirectMessage(_ context.Context, msg dm.SendInput) (bool, error) {
	return msg.Content != "reject", nil
}

func TestFeedMessage(t *testing.T) {
	t.Run("memory", func(t *testing.T) { testFeedMessage(t, app.Config{}) })
	t.Run("postgres", func(t *testing.T) { config, _ := postgresTestDatabase(t); testFeedMessage(t, config) })
}

func testFeedMessage(t *testing.T, config app.Config) {
	config.DMModerator = feedAudit{}
	open := func() *app.App {
		t.Helper()
		a, err := app.New(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(a.Close)
		return a
	}
	a := open()
	fan := subscriptionTestUser(t, a, "fan", false)
	anchor := subscriptionTestUser(t, a, "anchor", true)
	other := subscriptionTestUser(t, a, "other", false)
	const base = "/api/feed/message/"
	uid := strconv.FormatInt(anchor.User.ID, 10)
	fid := strconv.FormatInt(fan.User.ID, 10)
	post := func(user auth.Session, action string, body map[string]any) map[string]any {
		t.Helper()
		return authFlowRequest(t, a.Server.Handler, base+action, user.Token, body, 200)
	}
	get := func(user auth.Session, action string) any {
		t.Helper()
		return subscriptionGet(t, a, user, base+action)["data"]
	}
	if c := get(anchor, "config").(map[string]any); c["dating"] != false || c["dated"] != true {
		t.Fatal(c)
	}
	welcome := map[string]any{"data": "ようこそ"}
	if r := post(fan, "welcome", welcome); r["status"] != "4005" {
		t.Fatal(r)
	}
	if r := post(anchor, "welcome", map[string]any{"data": "reject"}); r["status"] != "4062" {
		t.Fatal(r)
	}
	post(anchor, "welcome", welcome)
	post(anchor, "config", map[string]any{"dating": true, "dated": false})
	post(anchor, "welcome", map[string]any{"data": ""})
	authFlowRequest(t, a.Server.Handler, "/api/sub/free_sub", fan.Token, map[string]any{"anchor_id": uid}, 200)
	id, can, err := a.DM.FindDMConversation(t.Context(), fan.User.ID, anchor.User.ID)
	if err != nil || !can {
		t.Fatalf("subscription: %v %v", can, err)
	}
	messages, err := a.DM.DMMessages(t.Context(), fan.User.ID, id, dm.NewPageRequest(1, 20))
	if err != nil || len(messages.Data) != 1 || messages.Data[0].Content != "ようこそ" {
		t.Fatalf("welcome: %+v %v", messages, err)
	}
	if result := post(fan, "enabled/"+uid, nil); result["data"] != true {
		t.Fatal(result)
	}
	if result := post(other, "enabled/"+uid, nil); result["data"] != false {
		t.Fatal(result)
	}
	rows := post(fan, "search", map[string]any{"page": 999, "name": "An"})
	if rows["page"] != float64(1) || rows["pages"] != float64(1) || len(rows["data"].([]any)) != 1 {
		t.Fatal(rows)
	}
	person := rows["data"].([]any)[0].(map[string]any)
	if person["uid"] != uid || person["display_name"] != "anchor" || person["plan_price"] != "0" {
		t.Fatal(person)
	}
	if rows := post(anchor, "search", map[string]any{"page": 1, "list_type": 1}); rows["data"].([]any)[0].(map[string]any)["uid"] != fid {
		t.Fatal(rows)
	}
	if rows := post(other, "search", map[string]any{"page": 1}); rows["pages"] != float64(0) || len(rows["data"].([]any)) != 0 {
		t.Fatal(rows)
	}
	authFlowRequest(t, a.Server.Handler, base+"search", anchor.Token, map[string]any{"page": 1}, 400)
	if r := post(fan, "inbox", map[string]any{"uid": uid, "data": "reject"}); r["status"] != "4061" {
		t.Fatal(r)
	}
	for _, msg := range []string{"one", "two"} {
		post(fan, "inbox", map[string]any{"uid": uid, "data": msg})
	}
	if config.DatabaseURL != "" {
		a = open()
	}
	if c := get(anchor, "config").(map[string]any); c["dated"] != false || c["dating"] != true {
		t.Fatal(c)
	}
	list := get(anchor, "inbox").([]any)
	if len(list) != 1 {
		t.Fatal(list)
	}
	entry := list[0].(map[string]any)
	if entry["uid"] != fid || entry["data"] != "two" || entry["can_message"] != true || entry["unreads"] != float64(2) {
		t.Fatal(entry)
	}
	if n := get(other, "inbox_unreads"); n != float64(0) {
		t.Fatal(n)
	}
	post(anchor, "inbox/read_all/"+fid, nil)
	if n := get(anchor, "inbox_unreads"); n != float64(0) {
		t.Fatal(n)
	}
	req := httptest.NewRequest(http.MethodDelete, base+"inbox/"+fid, nil)
	req.Header.Set("Authorization", "Bearer "+anchor.Token)
	w := httptest.NewRecorder()
	a.Server.Handler.ServeHTTP(w, req)
	var status map[string]any
	if json.Unmarshal(w.Body.Bytes(), &status) != nil || status["status"] != "200" {
		t.Fatalf("hide: %d %s", w.Code, w.Body.String())
	}
	if len(get(anchor, "inbox").([]any)) != 0 || len(get(fan, "inbox").([]any)) != 1 {
		t.Fatal("hide should affect only its owner")
	}
	post(fan, "inbox", map[string]any{"uid": uid, "data": "new"})
	if n := get(anchor, "inbox_unreads"); n != float64(1) {
		t.Fatal(n)
	}
}
