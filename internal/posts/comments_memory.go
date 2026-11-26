package posts

import (
	"cmp"
	"context"
	"slices"
	"time"
)

func (m *Memory) interactive(ctx context.Context, actor, postID int64) (Post, error) {
	p, err := m.get(ctx, actor, postID)
	if err == nil && (!p.Full || p.Status != Approved) {
		err = ErrForbidden
	}
	return p, err
}

func (m *Memory) CreatePostComment(ctx context.Context, comment Comment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, err := m.interactive(ctx, comment.AuthorID, comment.PostID)
	if err != nil {
		return err
	}
	if comment.ParentID != nil {
		parent, ok := m.comments[*comment.ParentID]
		if !ok || parent.PostID != comment.PostID || parent.Deleted {
			return ErrNotFound
		}
	}
	m.nextComment++
	comment.ID = m.nextComment
	comment.CreatedAt = time.Now().UTC()
	m.comments[comment.ID] = comment
	p.CommentCount++
	m.posts[p.ID] = p
	return nil
}

func (m *Memory) PostComments(ctx context.Context, actor, id int64) ([]Comment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.interactive(ctx, actor, id); err != nil {
		return nil, err
	}
	result := []Comment{}
	for _, item := range m.comments {
		if item.PostID == id && !item.Deleted {
			_, item.Liked = m.commentLikes[item.ID][actor]
			result = append(result, item)
		}
	}
	slices.SortFunc(result, func(a, b Comment) int {
		if order := b.CreatedAt.Compare(a.CreatedAt); order != 0 {
			return order
		}
		return cmp.Compare(b.ID, a.ID)
	})
	return result, nil
}

func (m *Memory) DeletePostComment(ctx context.Context, actor, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	root, ok := m.comments[id]
	if !ok || root.Deleted {
		return ErrNotFound
	}
	p, err := m.get(ctx, actor, root.PostID)
	if err != nil {
		return err
	}
	if root.AuthorID != actor && p.OwnerID != actor {
		return ErrForbidden
	}
	children := make(map[int64][]int64)
	for _, item := range m.comments {
		if item.PostID == p.ID && !item.Deleted && item.ParentID != nil {
			children[*item.ParentID] = append(children[*item.ParentID], item.ID)
		}
	}
	queue := []int64{root.ID}
	for len(queue) != 0 {
		id := queue[0]
		queue = append(queue[1:], children[id]...)
		item := m.comments[id]
		item.Deleted = true
		m.comments[id] = item
		p.CommentCount--
	}
	m.posts[p.ID] = p
	return nil
}

func setMemoryLike(likes map[int64]map[int64]time.Time, actor, id int64, liked bool) int64 {
	if !liked {
		delete(likes[id], actor)
		return int64(len(likes[id]))
	}
	if likes[id] == nil {
		likes[id] = make(map[int64]time.Time)
	}
	if _, exists := likes[id][actor]; !exists {
		likes[id][actor] = time.Now().UTC()
	}
	return int64(len(likes[id]))
}

func (m *Memory) LikePost(ctx context.Context, actor, id int64, liked bool) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, err := m.interactive(ctx, actor, id)
	if err != nil {
		return 0, err
	}
	p.LikeCount = setMemoryLike(m.postLikes, actor, id, liked)
	m.posts[id] = p
	return p.LikeCount, nil
}

func (m *Memory) LikePostComment(ctx context.Context, actor, id int64, liked bool) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	comment, ok := m.comments[id]
	if !ok || comment.Deleted {
		return 0, ErrNotFound
	}
	if _, err := m.interactive(ctx, actor, comment.PostID); err != nil {
		return 0, err
	}
	comment.Likes = setMemoryLike(m.commentLikes, actor, id, liked)
	m.comments[id] = comment
	return comment.Likes, nil
}

func (m *Memory) PostReactions(ctx context.Context, actor, id int64, tips bool, offset, limit int) (Page[Reaction], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.interactive(ctx, actor, id); err != nil {
		return Page[Reaction]{}, err
	}
	rows := []Reaction{}
	if !tips {
		for uid, at := range m.postLikes[id] {
			rows = append(rows, Reaction{UserID: uid, CreatedAt: at})
		}
	}
	slices.SortFunc(rows, func(a, b Reaction) int {
		if order := b.CreatedAt.Compare(a.CreatedAt); order != 0 {
			return order
		}
		return cmp.Compare(b.UserID, a.UserID)
	})
	start := min(offset, len(rows))
	return Page[Reaction]{Rows: rows[start:min(start+limit, len(rows))], Total: int64(len(rows))}, nil
}
