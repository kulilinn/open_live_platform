package app_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"open_live_platform/internal/app"
	"open_live_platform/internal/posts"
)

func TestPostgresStaticAuditBindsTheExactSubjectVersion(t *testing.T) {
	config, db := postgresTestDatabase(t)
	var mu sync.Mutex
	type review struct {
		posts.ReviewRequest
		requestID, content string
	}
	var requests []review
	auditor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input posts.ReviewRequest
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer review-upstream" || json.NewDecoder(r.Body).Decode(&input) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		content := input.Text
		if len(input.Media) != 0 {
			content = input.Media[0].URL
		}
		entry := review{input, fmt.Sprintf("static-%d", len(requests)+1), content}
		requests = append(requests, entry)
		fmt.Fprintf(w, `{"pass_status":3,"manual_request_id":%q}`, entry.requestID)
	}))
	t.Cleanup(auditor.Close)
	config.PostAuditURL, config.PostAuditToken, config.PostReviewToken = auditor.URL, "review-upstream", "static-callback"
	open := func() *app.App {
		a, err := app.New(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(a.Close)
		return a
	}
	first, second := open(), open()
	owner := subscriptionTestUser(t, first, "static-anchor", true)
	other := subscriptionTestUser(t, second, "static-other", true)
	uid := strconv.FormatInt(owner.User.ID, 10)
	if _, err := db.Exec(t.Context(), `INSERT INTO anchor_profiles(user_id,state) VALUES($1,1) ON CONFLICT(user_id) DO UPDATE SET state=1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(t.Context(), `UPDATE users SET profile=coalesce(profile,'{}'::jsonb)||'{"IsAnchor":true}' WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	send := func(a *app.App, path string, body any) map[string]any {
		t.Helper()
		out := anchorPostRequest(t, a, "POST", path, owner.Token, "", body, http.StatusOK)
		if out["status"] != "200" {
			t.Fatalf("%s returned %+v", path, out)
		}
		return out
	}
	find := func(kind, content string, n int) review {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		for _, entry := range requests {
			if entry.Kind == kind && entry.content == content {
				if n == 0 {
					return entry
				}
				n--
			}
		}
		t.Fatalf("no review for %s %q", kind, content)
		return review{}
	}
	callback := func(a *app.App, entry review, kind int, token, ownerID, objectID, content string, code int) {
		t.Helper()
		result := anchorPostRequest(t, a, "POST", "/sm/static/callback", token, "", map[string]any{
			"requestId": entry.requestID, "result": map[string]int{"operation": 2},
			"data": map[string]any{"passThrough": map[string]any{"userId": ownerID, "id": objectID, "contentType": kind, "content": content}},
		}, code)
		if code == http.StatusOK && result["code"] != float64(1100) {
			t.Fatalf("bad audit acknowledgement: %+v", result)
		}
	}
	apply := func(entry review, kind int) {
		t.Helper()
		callback(second, entry, kind, config.PostReviewToken, uid, strconv.FormatInt(entry.PostID, 10), entry.content, 200)
	}
	const settings = "/api/live/room/setting"
	const photo = "https://media.invalid/current.jpg"
	const banner = "https://media.invalid/banner.jpg"
	send(first, "/anchor/api/set_info", map[string]string{"display_name": "Updated", "search_name": "staticprofile", "img": photo, "intro": "Biographical text"})
	send(first, settings, map[string]string{"title": "First draft", "banner": banner, "offlineInfo": "Offline text"})
	send(second, settings, map[string]string{"title": "Replacement draft"})
	send(first, settings, map[string]string{"title": "First draft"})
	old, current := find("room_title", "First draft", 0), find("room_title", "First draft", 1)
	callback(second, current, posts.ReviewRoomTitle, owner.Token, uid, uid, current.content, 401)
	callback(second, current, posts.ReviewRoomTitle, config.PostReviewToken, strconv.FormatInt(other.User.ID, 10), uid, current.content, 404)
	callback(second, current, posts.ReviewRoomTitle, config.PostReviewToken, uid, "999", current.content, 404)
	callback(second, current, posts.ReviewRoomTitle, config.PostReviewToken, uid, uid, "altered", 404)
	apply(old, posts.ReviewRoomTitle)
	room, err := second.Rooms.Info(t.Context(), owner.User.ID)
	if err != nil || room.Title != "First draft" {
		t.Fatalf("version replay changed matching content: %+v %v", room, err)
	}
	apply(current, posts.ReviewRoomTitle)
	apply(current, posts.ReviewRoomTitle)
	apply(find("room_banner", banner, 0), posts.ReviewRoomBanner)
	apply(find("room_offline_info", "Offline text", 0), posts.ReviewOfflineInfo)
	room, err = first.Rooms.Info(t.Context(), owner.User.ID)
	if err != nil || room.Title != "" || room.Banner != photo || room.OfflineInfo != "" {
		t.Fatalf("current room review did not apply source replacements: %+v %v", room, err)
	}
	apply(find("anchor_intro", "Biographical text", 0), posts.ReviewIntro)
	apply(find("anchor_avatar", photo, 0), posts.ReviewAvatar)
	apply(find("anchor_display_name", "Updated", 0), posts.ReviewDisplayName)
	apply(find("anchor_search_name", "staticprofile", 0), posts.ReviewSearchName)
	var account, search, avatar, intro string
	if err := db.QueryRow(t.Context(), `SELECT u.account,u.profile->>'SearchName',u.profile->>'Photo',a.details->>'Intro'
		FROM users u JOIN anchor_profiles a ON a.user_id=u.id WHERE u.id=$1`, owner.User.ID).Scan(&account, &search, &avatar, &intro); err != nil ||
		!strings.HasPrefix(account, "User") || len(account) != 11 || !strings.HasPrefix(search, "t") || len(search) != 9 || avatar != "" || intro != "" {
		t.Fatalf("profile actions account=%q search=%q avatar=%q intro=%q error=%v", account, search, avatar, intro, err)
	}
	const planName = "Fan plan"
	plan := send(first, "/api/sub/anchor/create_pay", map[string]any{"name": "Initial plan", "price": 990})["result"].(map[string]any)["plan_id"].(string)
	send(first, "/api/sub/anchor/edit", map[string]any{"plan_id": plan, "name": planName, "price": 990})
	apply(find("subscription_plan_name", planName, 0), posts.ReviewPlanName)
	send(second, "/api/date/set/wheel/value", map[string]any{"anchor_id": uid, "value": []string{"Wheel one", "Wheel two"}})
	apply(find("wheel_option", "Wheel two", 0), posts.ReviewWheelOption)
	apply(find("wheel_option", "Wheel one", 0), posts.ReviewWheelOption)
	send(first, "/api/feed/message/welcome", map[string]string{"data": "Welcome message"})
	apply(find("subscription_welcome", "Welcome message", 0), posts.ReviewWelcome)
	var name, welcome string
	var options []string
	if err := db.QueryRow(t.Context(), `SELECT (SELECT name FROM subscription_plans WHERE id=$2::text::bigint),
		(SELECT welcome FROM message_config WHERE user_id=$1),(SELECT options FROM room_wheel_configs WHERE anchor_id=$1)`, owner.User.ID, plan).
		Scan(&name, &welcome, &options); err != nil || name != "" || welcome != "" || len(options) != 2 || options[0] != "******" || options[1] != "******" {
		t.Fatalf("plan, welcome or wheel rejection: %q %q %v (%v)", name, welcome, options, err)
	}
	var titleNotices, oldApplied int
	if err := db.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM notices WHERE receive_id=$1 AND notice_type=22),
		(SELECT count(*) FROM static_review_requests WHERE request_id=$2 AND applied)`, owner.User.ID, old.requestID).
		Scan(&titleNotices, &oldApplied); err != nil || titleNotices != 1 || oldApplied != 0 {
		t.Fatalf("static review history notices=%d stale=%d error=%v", titleNotices, oldApplied, err)
	}
	participant := subscriptionTestUser(t, first, "static-fan", false)
	public := anchorPostRequest(t, first, "POST", "/api/user/set_info", participant.Token, "", map[string]any{
		"displayName": "User review name", "searchName": "reviewedfan", "img": photo, "banner": banner, "trans": true, "transLang": "ja",
	}, http.StatusOK)
	if public["status"] != "200" {
		t.Fatalf("public profile edit: %+v", public)
	}
	entry := find("user_display_name", "User review name", 0)
	fanID := strconv.FormatInt(participant.User.ID, 10)
	callback(second, entry, posts.ReviewDisplayName, config.PostReviewToken, fanID, fanID, entry.content, 200)
	fan, found, err := second.Accounts.Find(t.Context(), participant.User.ID)
	if err != nil || !found || fan.Profile == nil || !strings.HasPrefix(fan.Account, "User") || len(fan.Account) != 11 ||
		fan.Profile.SearchName != "reviewedfan" || fan.Profile.Banner != banner || !fan.TranslationEnabled || fan.TranslationLanguage != "ja" {
		t.Fatalf("public profile rejection affected other fields: %+v %v", fan, err)
	}
}

func TestPostgresStaticAuditUpgradesOlderMessageConfig(t *testing.T) {
	config, db := postgresTestDatabase(t)
	ctx := t.Context()
	baseline, err := app.New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	user := subscriptionTestUser(t, baseline, "static-migration", false)
	baseline.Close()
	if _, err := db.Exec(ctx, `DROP FUNCTION track_static_content CASCADE;
		DROP FUNCTION bump_static_review; DROP TABLE static_review_requests,static_review_versions;
		ALTER TABLE message_config DROP COLUMN welcome; DELETE FROM schema_migrations WHERE version=102`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO message_config(user_id,dating,dated) VALUES($1,true,false)`, user.User.ID); err != nil {
		t.Fatal(err)
	}
	upgraded, err := app.New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(upgraded.Close)
	var dating, dated bool
	var welcome string
	if err := db.QueryRow(ctx, `SELECT dating,dated,welcome FROM message_config WHERE user_id=$1`, user.User.ID).Scan(&dating, &dated, &welcome); err != nil ||
		!dating || dated || welcome != "" {
		t.Fatalf("legacy config upgrade changed values: %v %v %q (%v)", dating, dated, welcome, err)
	}
}
