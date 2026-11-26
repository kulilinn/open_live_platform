package app_test

import (
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
)

func TestSubscriptionDirectory(t *testing.T) {
	t.Run("memory", func(t *testing.T) { testSubscriptionDirectory(t, app.Config{}, nil) })
	t.Run("postgres", func(t *testing.T) {
		config, db := postgresTestDatabase(t)
		testSubscriptionDirectory(t, config, db)
	})
}

func testSubscriptionDirectory(t *testing.T, config app.Config, db *pgx.Conn) {
	t.Helper()
	platform, err := app.New(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(platform.Close)
	fan := subscriptionTestUser(t, platform, "directory-fan", false)
	anchor := subscriptionTestUser(t, platform, "directory-anchor", true)
	other := subscriptionTestUser(t, platform, "directory-other", false)
	anchorID := strconv.FormatInt(anchor.User.ID, 10)
	request := func(account auth.Session, method, route string, input any, statusCode int) map[string]any {
		t.Helper()
		return anchorPostRequest(t, platform, method, "/api/sub/"+route, account.Token, "", input, statusCode)
	}
	post := func(account auth.Session, route string, input any) map[string]any {
		t.Helper()
		return request(account, "POST", route, input, http.StatusOK)
	}
	groups := func(account auth.Session, kind int) map[string]any {
		t.Helper()
		return post(account, "get_groups", map[string]int{"ty": kind})["result"].(map[string]any)["groups"].(map[string]any)
	}
	firstDefaults := groups(fan, 0)
	if len(firstDefaults) != 1 || len(groups(other, 3)) != 0 {
		t.Fatal("wrong default groups")
	}
	for id := range firstDefaults {
		if _, found := groups(fan, 0)[id]; !found {
			t.Fatal("default groups were duplicated")
		}
	}
	post(fan, "create_group", map[string]string{"group_name": "favorites"})
	group := ""
	for id := range groups(fan, 3) {
		group = id
	}
	if group == "" {
		t.Fatal("custom group not created")
	}
	rename := map[string]string{"group_id": group, "group_name": "revised"}
	if result := post(other, "rename_group", rename); result["status"] != "4005" {
		t.Fatalf("other user renamed a group: %+v", result)
	}
	post(fan, "rename_group", rename)
	member := map[string]string{"group_id": group, "uid": anchorID}
	if result := post(other, "add_group", member); result["status"] != "4005" {
		t.Fatalf("other user changed membership: %+v", result)
	}
	for range 2 {
		post(fan, "add_group", member)
	}
	groupView := groups(fan, 3)[group].([]any)
	if groupView[0] != "revised" || len(groupView[1].([]any)) != 1 {
		t.Fatalf("group preview = %+v", groupView)
	}
	list := func(account auth.Session) []any {
		t.Helper()
		return post(account, "anchor_list", map[string]string{"group_id": group})["data"].(map[string]any)["anchors"].([]any)
	}
	if len(list(other)) != 0 || len(list(fan)) != 1 {
		t.Fatal("unexpected group member access")
	}
	post(fan, "remove_group", member)
	if len(list(fan)) != 0 {
		t.Fatal("member removal did not persist")
	}
	post(fan, "add_group", member)
	post(fan, "clear_group", map[string]string{"group_id": group})
	if len(list(fan)) != 0 || groups(fan, 3)[group].([]any)[0] != "revised" {
		t.Fatal("clearing a group failed")
	}
	post(fan, "add_group", member)
	friend := map[string]any{"uid": anchorID, "ty": 2}
	blacklist := map[string]any{"uid": anchorID, "ty": 3}
	post(fan, "add_group", friend)
	friendList := func() []any {
		t.Helper()
		return post(fan, "friend_list", nil)["data"].([]any)
	}
	blocked := func() []any {
		t.Helper()
		return post(fan, "blacklist", nil)["data"].(map[string]any)["list"].([]any)
	}
	if len(friendList()) != 1 {
		t.Fatal("friend list did not persist")
	}
	sub := map[string]string{"anchor_id": anchorID, "plan_id": "0"}
	post(fan, "free_sub", sub)
	if index := post(fan, "index", nil)["data"].([]any); len(index) != 1 || index[0].(map[string]any)["anchor_id"] != anchorID {
		t.Fatalf("active subscribed anchors = %+v", index)
	}
	post(fan, "add_group", blacklist)
	if len(friendList()) != 0 || len(blocked()) != 1 || len(list(fan)) != 0 ||
		len(post(fan, "index", nil)["data"].([]any)) != 0 {
		t.Fatal("blacklist failed to hide the member from the user's lists")
	}
	request(fan, "POST", "add_group", member, http.StatusForbidden)
	if len(post(fan, "get_sub", sub)["data"].([]any)) != 1 {
		t.Fatal("blacklist modified an existing subscription")
	}
	post(fan, "remove_group", blacklist)
	if len(blocked()) != 0 {
		t.Fatal("blacklist removal did not persist")
	}
	post(fan, "add_group", friend)
	post(fan, "del_group", map[string]int{"ty": 2})
	post(fan, "del_group", map[string]string{"group_id": group})
	if len(friendList()) != 0 || len(groups(fan, 3)) != 0 {
		t.Fatal("group deletion did not persist")
	}
	for _, route := range []string{"sub_statistics", "anchor/fans_statistics?query_id=" + anchorID} {
		counts := request(fan, "GET", route, nil, http.StatusOK)["data"].(map[string]any)
		if counts["all"] != float64(1) || counts["free"] != float64(1) || counts["pay"] != float64(0) {
			t.Fatalf("%s statistics = %+v", route, counts)
		}
	}
	route := "fans_pren?anchor_id=" + anchorID + "&page=1&page_size=10&level=0"
	if recent := request(other, "GET", route, nil, http.StatusOK)["data"].([]any); len(recent) != 1 {
		t.Fatalf("recent fans = %+v", recent)
	}
	request(other, "GET", route+"&is_cancel=true", nil, http.StatusForbidden)
	level := request(fan, "GET", "level/"+anchorID, nil, http.StatusOK)["data"].(map[string]any)
	if level["fans_level"] != float64(0) || len(level["subscription"].([]any)) != 1 {
		t.Fatalf("room subscription level = %+v", level)
	}
	for _, route := range []string{"upgrade", "downgrade"} {
		if result := post(fan, route, nil); result["status"] != "200" {
			t.Fatalf("legacy plan request = %+v", result)
		}
	}
	nameSearch := post(fan, "search", map[string]string{"user_name": "directory-anchor"})["data"].(map[string]any)
	if len(nameSearch["users"].([]any)) != 1 {
		t.Fatalf("subscription search = %+v", nameSearch)
	}
	post(fan, "report", map[string]any{"ty": 5, "url": []string{"https://evidence.invalid/example.png"}, "user_id": anchorID, "remark": "example"})
	if db != nil {
		var count int
		err := db.QueryRow(t.Context(), `SELECT count(*) FROM subscription_reports WHERE reporter_id=$1 AND person_id=$2 AND kind=5 AND remark='example'`,
			fan.User.ID, anchor.User.ID).Scan(&count)
		if err != nil || count != 1 {
			t.Fatalf("stored reports = %d, err=%v", count, err)
		}
	}
	request(fan, "POST", "report", map[string]any{"ty": 5, "url": []string{"javascript:alert(1)"}}, http.StatusBadRequest)
	fanLevel := request(fan, "GET", fmt.Sprintf("fans_level/%s", anchorID), nil, http.StatusOK)["data"].(map[string]any)
	if fanLevel["level"] != float64(0) {
		t.Fatalf("unpaid fan level = %+v", fanLevel)
	}
}
