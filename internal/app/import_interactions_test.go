package app_test

import (
	"encoding/json"
	"errors"
	"iter"
	"testing"
	"time"

	"open_live_platform/internal/dm"
	"open_live_platform/internal/notice"
	"open_live_platform/internal/store/postgres"
)

func TestPostgresImportLiveMediaInteractions(t *testing.T) {
	config, db := postgresTestDatabase(t)
	ctx := t.Context()
	store, err := postgres.Open(ctx, config.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Shutdown()
	ids := []int64{}
	for _, email := range []string{"fan@example.org", "anchor@example.org", "other@example.org"} {
		user, err := store.SignUp(ctx, email, "password", email)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, user.User.ID)
	}
	fan, anchor, other := ids[0], ids[1], ids[2]
	now := time.Now().UTC().Truncate(time.Microsecond)
	expires := now.Add(time.Hour)
	const conversationID int64 = 79591199061446658
	legacy := postgres.LegacyDMConversation{ID: conversationID, Type: 1, Members: []int64{anchor, fan}, Hidden: []int64{fan},
		Metadata:    dm.Metadata{Fan: dm.User{ID: fan, DisplayName: "Fan"}, Anchor: dm.User{ID: anchor, DisplayName: "Anchor", Anchor: true}},
		LastMessage: "こんにちは", LastActivity: now, CreatedAt: now.Add(-time.Hour), CanMessage: true, EntitlementUntil: &expires}
	firstNotice := postgres.LegacyNotice{Notice: notice.Notice{ID: 51, UserID: fan, Status: 1, Target: 1, Type: 2,
		Content: json.RawMessage(`{"displayName":"Anchor"}`), CreatedAt: now}}
	message := postgres.LegacyDMMessage{ID: 79591199061446659, ConversationID: conversationID, SenderID: anchor,
		ReceiverID: fan, Status: dm.Delivered, Type: 1, Content: "こんにちは", CreatedAt: now}
	wrongRecipient := message
	wrongRecipient.ID++
	wrongRecipient.ReceiverID = other
	importRows := func() postgres.InteractionImportCounts {
		t.Helper()
		result, err := store.ImportLegacyInteractions(ctx, legacyRows(legacy, postgres.LegacyDMConversation{Type: 2}),
			legacyRows(message, wrongRecipient), legacyRows(firstNotice, postgres.LegacyNotice{Deleted: true}))
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if result := importRows(); result != (postgres.InteractionImportCounts{Conversations: 1, Messages: 1, Notices: 1, Skipped: 3}) {
		t.Fatalf("import summary = %+v", result)
	}
	if id, allowed, err := store.FindDMConversation(ctx, fan, anchor); err != nil || id != conversationID || !allowed {
		t.Fatalf("imported conversation = %d, %t, %v", id, allowed, err)
	}
	if page, err := store.DMConversations(ctx, fan, dm.NewPageRequest(1, 20)); err != nil || len(page.Data) != 0 {
		t.Fatalf("legacy hidden conversation exposed: %v, %v", page, err)
	}
	if page, err := store.DMConversations(ctx, anchor, dm.NewPageRequest(1, 20)); err != nil || len(page.Data) != 1 || page.Data[0].LastMessage != legacy.LastMessage {
		t.Fatalf("anchor cannot see imported conversation: %v, %v", page, err)
	}
	if _, err := store.DMMessages(ctx, other, conversationID, dm.NewPageRequest(1, 20)); !errors.Is(err, dm.ErrNotFound) {
		t.Fatalf("third party can read historical messages: %v", err)
	}
	if err := store.ReadDM(ctx, fan, conversationID); err != nil {
		t.Fatal(err)
	}
	if err := store.ReadNotices(ctx, fan); err != nil {
		t.Fatal(err)
	}
	if result := importRows(); result != (postgres.InteractionImportCounts{Existing: 3, Skipped: 3}) {
		t.Fatalf("retry summary = %+v", result)
	}
	if messages, err := store.DMMessages(ctx, fan, conversationID, dm.NewPageRequest(1, 20)); err != nil || len(messages.Data) != 1 ||
		messages.Data[0].ID != message.ID || messages.Data[0].Status != dm.Read || messages.Data[0].Content != message.Content {
		t.Fatalf("message history after retry = %v, %v", messages, err)
	}
	if notices, err := store.Notices(ctx, fan, 1, 20); err != nil || len(notices.List) != 1 || notices.List[0].ID != firstNotice.ID || notices.List[0].Status != 2 {
		t.Fatalf("notice history after retry = %v, %v", notices, err)
	}
	if _, err := db.Exec(ctx, `UPDATE dm_conversations SET subscription_expires_at=now() WHERE id=$1`, conversationID); err != nil {
		t.Fatal(err)
	}
	if _, allowed, err := store.FindDMConversation(ctx, fan, anchor); err != nil || allowed {
		t.Fatalf("expired imported entitlement can still send direct messages: %t, %v", allowed, err)
	}
	if next, err := store.SetDMSubscription(ctx, dm.Metadata{Fan: dm.User{ID: other}, Anchor: dm.User{ID: anchor, Anchor: true}}, true, ""); err != nil || next <= conversationID {
		t.Fatalf("next conversation ID = %d, error = %v", next, err)
	}
}

func legacyRows[T any](rows ...T) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		for _, row := range rows {
			if !yield(row, nil) {
				return
			}
		}
	}
}
