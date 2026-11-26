package posts

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"open_live_platform/internal/subscription"
)

type Memory struct {
	mu                  sync.Mutex
	subscriptions       subscription.Store
	nextID, nextComment int64
	posts               map[int64]Post
	comments            map[int64]Comment
	postLikes           map[int64]map[int64]time.Time
	commentLikes        map[int64]map[int64]time.Time
}

func NewMemory(subscriptions subscription.Store) *Memory {
	return &Memory{subscriptions: subscriptions, posts: make(map[int64]Post), comments: make(map[int64]Comment),
		postLikes: make(map[int64]map[int64]time.Time), commentLikes: make(map[int64]map[int64]time.Time)}
}

func (m *Memory) CreatePost(ctx context.Context, actor int64, input Input, status int) (Post, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Post{}, err
	}
	m.nextID++
	input.Media = slices.Clone(input.Media)
	p := Post{Input: input, ID: m.nextID, OwnerID: actor, Status: status, CreatedAt: time.Now().UTC(), Full: true}
	m.posts[p.ID] = p
	return p, nil
}

func (m *Memory) ReviewPost(ctx context.Context, id int64, status int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	p, ok := m.posts[id]
	if !ok || p.Deleted {
		return ErrNotFound
	}
	if p.Status != Pending && p.Status != status {
		return ErrReviewConflict
	}
	p.Status = status
	m.posts[id] = p
	return nil
}

func (m *Memory) GetPost(ctx context.Context, actor, id int64) (Post, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.get(ctx, actor, id)
}

func (m *Memory) get(ctx context.Context, actor, id int64) (Post, error) {
	if err := ctx.Err(); err != nil {
		return Post{}, err
	}
	p, ok := m.posts[id]
	if !ok || !p.Visible(actor) {
		return Post{}, ErrNotFound
	}
	p.Full = p.OwnerID == actor || (p.Level == 0 && p.Price == 0)
	if !p.Full && p.Level >= subscription.Free {
		subs, err := m.subscriptions.Relationships(ctx, actor, p.OwnerID)
		if err != nil {
			return Post{}, err
		}
		now := time.Now()
		for _, sub := range subs {
			if sub.AnchorID == p.OwnerID && sub.FanID == actor && sub.Level >= 0 && sub.Level < 3 && sub.Level+subscription.Free >= p.Level && sub.Active(now) {
				p.Full = true
				break
			}
		}
	}
	p.Media = slices.Clone(p.Media)
	_, p.Liked = m.postLikes[id][actor]
	return p, nil
}

func (m *Memory) ListPosts(ctx context.Context, f Filter) (Page[Post], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Page[Post]{}, err
	}
	rows := []Post{}
	var total int64
	for id, raw := range m.posts {
		if !raw.Visible(f.Viewer) || (f.Owner != 0 && raw.OwnerID != f.Owner) || (f.Owner == 0 && raw.Status != Approved) || (raw.Level == 1) != f.OwnOnly {
			continue
		}
		p, err := m.get(ctx, f.Viewer, id)
		if err != nil {
			return Page[Post]{}, err
		}
		image, video := p.Images > 0, p.Videos > 0
		if (f.Media == 1 && !image) || (f.Media == 2 && !video) || (f.Media == 3 && !image && !video) ||
			(f.Kind == 1 && !p.Bought) || (f.Kind == 2 && p.Level != 3) || (f.Kind == 3 && p.Price == 0) ||
			(f.Kind == 4 && p.Level != 2) || (f.Kind == 5 && p.Level != 4) ||
			(f.Search != "" && (!(p.Full || p.OnlyText == 1) || !strings.Contains(strings.ToLower(p.Content), strings.ToLower(f.Search)))) {
			continue
		}
		total++
		if f.Before == 0 || id < f.Before {
			rows = append(rows, p)
		}
	}
	slices.SortFunc(rows, func(a, b Post) int {
		var order int
		switch f.Order {
		case 6:
			order = b.CreatedAt.Compare(a.CreatedAt)
		case 7:
			order = cmp.Compare(b.TipPoints, a.TipPoints)
		case 8:
			order = cmp.Compare(b.LikeCount, a.LikeCount)
		case 9:
			order = a.CreatedAt.Compare(b.CreatedAt)
		}
		if order != 0 {
			return order
		}
		return cmp.Compare(b.ID, a.ID)
	})
	start := min(f.Offset, len(rows))
	return Page[Post]{Rows: rows[start:min(start+f.Size, len(rows))], Total: total}, nil
}

func (m *Memory) DeletePost(ctx context.Context, actor, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	p, ok := m.posts[id]
	if !ok {
		return ErrNotFound
	}
	if p.OwnerID != actor {
		return ErrForbidden
	}
	if p.Deleted {
		return ErrDeleted
	}
	p.Deleted = true
	m.posts[id] = p
	return nil
}

func (m *Memory) PostStats(ctx context.Context, owner int64) (Stats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := Stats{}
	for _, p := range m.posts {
		if !p.Deleted && p.Status == Approved && p.Level != 1 && p.OwnerID == owner {
			result.Posts++
			result.Images += int64(p.Images)
			result.Videos += int64(p.Videos)
		}
	}
	return result, ctx.Err()
}

func (m *Memory) RecentPostAnchors(ctx context.Context, limit int) ([]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	latest := make(map[int64]time.Time)
	for _, p := range m.posts {
		if !p.Deleted && p.Status == Approved && p.Level != 1 && p.CreatedAt.After(latest[p.OwnerID]) {
			latest[p.OwnerID] = p.CreatedAt
		}
	}
	ids := make([]int64, 0, len(latest))
	for id := range latest {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b int64) int {
		if order := latest[b].Compare(latest[a]); order != 0 {
			return order
		}
		return cmp.Compare(b, a)
	})
	return ids[:min(limit, len(ids))], ctx.Err()
}

func (*Memory) PostBuyerCount(ctx context.Context, _ int64) (int64, error) { return 0, ctx.Err() }
