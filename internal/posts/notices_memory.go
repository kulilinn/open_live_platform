package posts

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/notice"
	"open_live_platform/internal/subscription"
)

type MemoryNoticeOptions struct {
	Accounts auth.AccountStore
	Notices  notice.Store
}

type memoryPostNotices struct {
	MemoryNoticeOptions
	lastID int64
	liked  map[[2]int64]struct{}
}

func (m *Memory) notifyPublished(ctx context.Context, post Post) error {
	if m.notifications.Notices == nil || post.Level == 1 {
		return nil
	}
	rows, err := m.subscriptions.Relationships(ctx, 0, post.OwnerID)
	if err != nil {
		return err
	}
	seen := make(map[int64]struct{})
	now := time.Now()
	for _, sub := range rows {
		if _, known := seen[sub.FanID]; known || !sub.Active(now) ||
			(post.Level >= subscription.Free && (sub.Level+subscription.Free < post.Level || sub.Level > 2)) {
			continue
		}
		seen[sub.FanID] = struct{}{}
		if err := m.notify(ctx, NoticePublished, post.ID, post.OwnerID, sub.FanID); err != nil {
			return err
		}
	}
	return nil
}

func (m *Memory) notifyLike(ctx context.Context, post Post, actor int64) error {
	if m.notifications.Notices == nil || actor == post.OwnerID {
		return nil
	}
	key := [2]int64{post.ID, actor}
	if _, seen := m.notifications.liked[key]; seen {
		return nil
	}
	if err := m.notify(ctx, NoticeLike, post.ID, actor, post.OwnerID); err != nil {
		return err
	}
	if m.notifications.liked == nil {
		m.notifications.liked = make(map[[2]int64]struct{})
	}
	m.notifications.liked[key] = struct{}{}
	return nil
}

func (m *Memory) notify(ctx context.Context, kind int8, post, actor, receiver int64) error {
	if m.notifications.Notices == nil || actor == receiver {
		return nil
	}
	blocked, err := m.subscriptions.SavedSubscriptionUsers(ctx, receiver, subscription.Blacklist)
	if err != nil {
		return err
	}
	if _, hidden := slices.BinarySearch(blocked, actor); hidden {
		return nil
	}
	sender, found, err := m.notifications.Accounts.Find(ctx, actor)
	if err != nil || !found {
		return err
	}
	photo := ""
	if sender.Profile != nil {
		photo = sender.Profile.Photo
	}
	field := "user_name"
	if kind == NoticePublished {
		field = "anchor_name"
	}
	content, err := json.Marshal(map[string]string{field: sender.Account})
	if err != nil {
		return err
	}
	m.notifications.lastID++
	return m.notifications.Notices.AddNotice(ctx, notice.Notice{ID: m.notifications.lastID, Target: 2, Type: kind,
		UserID: receiver, Avatar: &photo, Content: content, ObjectID: &post, CreatedAt: time.Now().UTC()})
}

func (m *Memory) notifyRejection(ctx context.Context, kind int8, receiver int64) error {
	if m.notifications.Notices == nil || kind == 0 {
		return nil
	}
	target := int8(1)
	if kind == NoticeCommentRejection {
		target = 3
	}
	m.notifications.lastID++
	return m.notifications.Notices.AddNotice(ctx, notice.Notice{ID: m.notifications.lastID, Target: target, Type: kind,
		UserID: receiver, Content: json.RawMessage(`{"key":"","content":""}`), CreatedAt: time.Now().UTC()})
}
