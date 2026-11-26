package notice

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"sync"
	"time"
)

type Notice struct {
	ID        int64           `json:"id,string"`
	Target    int8            `json:"noticeTarget"`
	Type      int8            `json:"noticeType"`
	UserID    int64           `json:"userId,string"`
	Avatar    *string         `json:"avatar"`
	Content   json.RawMessage `json:"content"`
	ObjectID  *int64          `json:"objectId,string"`
	Status    int8            `json:"status"`
	CreatedAt time.Time       `json:"createTime"`
}

type Page struct {
	List    []Notice `json:"list"`
	Current int32    `json:"current"`
	Size    int32    `json:"pageSize"`
	Total   int64    `json:"total"`
}

type Store interface {
	AddNotice(context.Context, Notice) error
	Notices(ctx context.Context, userID int64, page, size int32) (Page, error)
	ReadNotices(ctx context.Context, userID int64) error
	UnreadNotices(ctx context.Context, userID int64) (int64, error)
}

type Memory struct {
	mu   sync.RWMutex
	rows map[int64]Notice
}

func (m *Memory) AddNotice(_ context.Context, n Notice) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rows == nil {
		m.rows = make(map[int64]Notice)
	}
	if _, exists := m.rows[n.ID]; !exists {
		n.Status = 1
		m.rows[n.ID] = n
	}
	return nil
}

func (m *Memory) Notices(_ context.Context, userID int64, page, size int32) (Page, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rows := []Notice{}
	for _, n := range m.rows {
		if n.UserID == userID {
			rows = append(rows, n)
		}
	}
	slices.SortFunc(rows, func(a, b Notice) int {
		if order := b.CreatedAt.Compare(a.CreatedAt); order != 0 {
			return order
		}
		return cmp.Compare(b.ID, a.ID)
	})
	total := int64(len(rows))
	from := min(total, int64(page-1)*int64(size))
	return Page{rows[from:min(total, from+int64(size))], page, size, total}, nil
}

func (m *Memory) ReadNotices(_ context.Context, userID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, n := range m.rows {
		if n.UserID == userID && n.Status == 1 {
			n.Status = 2
			m.rows[id] = n
		}
	}
	return nil
}

func (m *Memory) UnreadNotices(_ context.Context, userID int64) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var count int64
	for _, n := range m.rows {
		if n.UserID == userID && n.Status == 1 {
			count++
		}
	}
	return count, nil
}
