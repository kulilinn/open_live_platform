package feedback

import (
	"context"
	"errors"
	"sync"
	"time"
)

var ErrTooOften = errors.New("提交太频繁")

type Record struct {
	ID, UserID          int64
	Type                uint8
	Content, Attachment string
	Status              int
	Created             time.Time
}
type Store interface {
	ReserveFeedback(context.Context, Record) (int64, error)
	DiscardFeedback(context.Context, int64) error
	FinishFeedback(context.Context, int64, int) error
}
type Memory struct {
	mu   sync.Mutex
	rows map[int64]Record
	last map[int64]int64
	next int64
}

func (m *Memory) ReserveFeedback(_ context.Context, r Record) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rows == nil {
		m.rows = map[int64]Record{}
		m.last = map[int64]int64{}
	}
	if old, ok := m.rows[m.last[r.UserID]]; ok && time.Since(old.Created) < time.Minute {
		return 0, ErrTooOften
	}
	m.next++
	r.ID, r.Created = m.next, time.Now().UTC()
	m.rows[r.ID], m.last[r.UserID] = r, r.ID
	return r.ID, nil
}
func (m *Memory) DiscardFeedback(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, id)
	return nil
}
func (m *Memory) FinishFeedback(_ context.Context, id int64, status int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := m.rows[id]
	v.Status = status
	m.rows[id] = v
	return nil
}
