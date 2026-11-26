package app_test

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"open_live_platform/internal/app"
	"open_live_platform/internal/posts"
	"open_live_platform/internal/store/postgres"
	"open_live_platform/internal/subscription"
)

func TestPostgresImportLiveMediaPublishing(t *testing.T) {
	config, db := postgresTestDatabase(t)
	platform, err := app.New(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(platform.Close)
	owner := subscriptionTestUser(t, platform, "publishing-owner", true)
	buyer := subscriptionTestUser(t, platform, "publishing-buyer", false)
	other := subscriptionTestUser(t, platform, "publishing-outsider", false)
	store, err := postgres.Open(t.Context(), config.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Shutdown)
	now := time.Now().UTC().Add(-time.Hour)
	const postID, rootID, childID, shareID, groupID int64 = 9007199254740993, 9007199254740995, 9007199254740996, 9007199254740997, 9007199254740998
	post := postgres.LegacyPost{Post: posts.Post{
		ID: postID, OwnerID: owner.User.ID, CreatedAt: now, Status: posts.Approved, TipPoints: 15,
		Input: posts.Input{Content: "historical post", Price: 20, Media: []posts.Media{{URL: "https://media.invalid/legacy.jpg"}}},
	}, Buyers: []int64{buyer.User.ID}}
	pending := postgres.LegacyPost{Post: posts.Post{
		ID: postID + 1, OwnerID: owner.User.ID, CreatedAt: now, Status: posts.Pending,
		Input: posts.Input{Content: "unapproved historical post", Media: []posts.Media{}},
	}}
	root := posts.Comment{ID: rootID, PostID: postID, AuthorID: buyer.User.ID, Content: "root comment", CreatedAt: now}
	parent := rootID
	child := posts.Comment{ID: childID, PostID: postID, AuthorID: owner.User.ID, Content: "child comment", ParentID: &parent, CreatedAt: now}
	postLike := postgres.LegacyPostReaction{TargetID: postID, ActorID: buyer.User.ID, CreatedAt: now}
	commentLike := postgres.LegacyPostReaction{TargetID: rootID, ActorID: owner.User.ID, CreatedAt: now}
	tip := postgres.LegacyPostReaction{TargetID: postID, ActorID: buyer.User.ID, CreatedAt: now, Points: 15}
	share := postgres.LegacyShare{ID: shareID, OwnerID: owner.User.ID, Content: "historical share", Photo: "previous.jpg",
		Comments: "[]", Likes: 3, Status: 2, CreatedAt: now, UpdatedAt: now}
	group := postgres.LegacyGroup{ID: groupID, OwnerID: buyer.User.ID, Kind: subscription.CustomGroup, Name: "old group", Members: []int64{owner.User.ID}}
	friend := postgres.LegacySavedUsers{OwnerID: buyer.User.ID, Kind: subscription.Friends, Members: []int64{owner.User.ID}}
	blocked := postgres.LegacySavedUsers{OwnerID: buyer.User.ID, Kind: subscription.Blacklist, Members: []int64{other.User.ID}}
	data := postgres.LegacyPublishing{
		Posts: legacyRows(post, pending), Comments: legacyRows(child, root), PostLikes: legacyRows(postLike),
		CommentLikes: legacyRows(commentLike), Tips: legacyRows(tip), Shares: legacyRows(share),
		Groups: legacyRows(group), Saved: legacyRows(friend, blocked),
	}
	counts, err := store.ImportLegacyPublishing(t.Context(), data)
	if err != nil || counts.Posts != 2 || counts.Comments != 2 || counts.PostLikes != 1 || counts.CommentLikes != 1 ||
		counts.Tips != 1 || counts.Unlocks != 1 || counts.Shares != 1 || counts.Groups != 1 || counts.Saved != 2 || counts.Skipped != 0 {
		t.Fatalf("publishing import = %+v, %v", counts, err)
	}
	counts, err = store.ImportLegacyPublishing(t.Context(), data)
	if err != nil || counts.Existing != 11 || counts.Skipped != 0 || counts.Unlocks != 0 {
		t.Fatalf("publishing replay = %+v, %v", counts, err)
	}
	id := strconv.FormatInt(postID, 10)
	path := "/anchor/api/get_one_post/" + id
	result := anchorPostRequest(t, platform, "GET", path, buyer.Token, "", nil, http.StatusOK)["data"].(map[string]any)
	if result["id"] != id || result["privilege"] != float64(1) || result["comment_num"] != float64(2) || result["like_num"] != float64(1) ||
		result["my_tip_amount"] != float64(15) {
		t.Fatalf("imported buyer view = %+v", result)
	}
	result = anchorPostRequest(t, platform, "GET", path, other.Token, "", nil, http.StatusOK)["data"].(map[string]any)
	if result["privilege"] != float64(0) {
		t.Fatalf("legacy buyer entitlement admitted a foreign user: %+v", result)
	}
	hidden := anchorPostRequest(t, platform, "GET", "/anchor/api/get_one_post/"+strconv.FormatInt(postID+1, 10), other.Token, "", nil, http.StatusOK)
	if hidden["data"] != nil {
		t.Fatalf("legacy pending article was published: %+v", hidden)
	}
	comments := anchorPostRequest(t, platform, "GET", "/anchor/api/get_post_comment?post_id="+id, owner.Token, "", nil, http.StatusOK)["data"].([]any)
	if len(comments) != 1 || comments[0].(map[string]any)["likes"] != float64(1) || len(comments[0].(map[string]any)["children"].([]any)) != 1 {
		t.Fatalf("historical comment tree = %+v", comments)
	}
	groups := anchorPostRequest(t, platform, "POST", "/api/sub/get_groups", buyer.Token, "", map[string]int{"ty": 3}, http.StatusOK)["result"].(map[string]any)["groups"].(map[string]any)
	if len(groups) != 1 || groups[strconv.FormatInt(groupID, 10)].([]any)[0] != "old group" {
		t.Fatalf("historical groups = %+v", groups)
	}
	var shares, finance, notices int
	err = db.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM public_shares WHERE id=$1 AND status=2 AND likes=3),
		(SELECT count(*) FROM anchor_post_orders),(SELECT count(*) FROM notices)`, shareID).Scan(&shares, &finance, &notices)
	if err != nil || shares != 1 || finance != 0 || notices != 0 {
		t.Fatalf("historical share/live finance/notices = %d/%d/%d error=%v", shares, finance, notices, err)
	}
}
