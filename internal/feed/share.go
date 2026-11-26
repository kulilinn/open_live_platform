package feed

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"
)

var ErrShareMissing = errors.New("share not found")

type Share struct {
	ID       int64     `json:"id"`
	UserID   int64     `json:"uid"`
	Content  string    `json:"content"`
	Comments string    `json:"comments"`
	Photo    string    `json:"photo"`
	Likes    int       `json:"likes"`
	Status   int       `json:"status"`
	Created  time.Time `json:"create_at"`
	Updated  time.Time `json:"update_at"`
	Version  *uint64   `json:"_version"`
}
type ShareStore interface {
	CreateShare(context.Context, int64, string, string) (Share, error)
	OwnsShare(context.Context, int64, int64) (bool, error)
	ChangeShare(ctx context.Context, uid, id int64, content, photo string, remove bool) error
	ListShares(ctx context.Context, offset int64, hot bool, owner int64) ([]Share, error)
}

func (m *Memory) CreateShare(_ context.Context, uid int64, content, photo string) (Share, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shares == nil {
		m.shares = make(map[int64]Share)
	}
	m.nextShare++
	now := time.Now().UTC()
	v := Share{ID: m.nextShare, UserID: uid, Content: content, Photo: photo, Comments: "[]", Status: 1, Created: now, Updated: now}
	m.shares[v.ID] = v
	return v, nil
}
func (m *Memory) OwnsShare(_ context.Context, uid, id int64) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.shares[id]
	return ok && v.UserID == uid && v.Status != 0, nil
}
func (m *Memory) ChangeShare(_ context.Context, uid, id int64, content, photo string, remove bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.shares[id]
	if !ok || v.UserID != uid || v.Status == 0 {
		return ErrShareMissing
	}
	v.Content, v.Updated = content, time.Now().UTC()
	if remove {
		v.Status = 0
	} else {
		v.Status, v.Photo = 2, photo
	}
	m.shares[id] = v
	return nil
}
func (m *Memory) ListShares(_ context.Context, offset int64, hot bool, owner int64) ([]Share, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := []Share{}
	for _, v := range m.shares {
		if v.Status > 0 && (owner == 0 || v.UserID == owner) {
			result = append(result, v)
		}
	}
	slices.SortFunc(result, func(a, b Share) int {
		if hot && a.Likes != b.Likes {
			return cmp.Compare(b.Likes, a.Likes)
		}
		if c := b.Created.Compare(a.Created); c != 0 {
			return c
		}
		return cmp.Compare(b.ID, a.ID)
	})
	start := min(int64(len(result)), offset)
	return result[start:min(start+100, int64(len(result)))], nil
}
