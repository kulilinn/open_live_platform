package feed

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"time"
)

type Config struct {
	Welcome string `json:"-"`
	Dating  bool   `json:"dating"`
	Dated   bool   `json:"dated"`
}
type Entry struct {
	Peer    int64
	Data    string
	Unreads int
	Updated time.Time
}
type Store interface {
	FeedConfig(context.Context, int64) (Config, error)
	SetFeedConfig(context.Context, int64, Config) error
	SetFeedWelcome(context.Context, int64, string) error
	FeedEntries(context.Context, int64) ([]Entry, error)
	FeedUnread(context.Context, int64) (int64, error)
	SetFeedEntry(ctx context.Context, uid, peer int64, data string) error
	MarkFeedEntry(ctx context.Context, uid, peer int64, hide bool) error
}

type row struct {
	Entry
	hidden bool
}
type Memory struct {
	mu     sync.RWMutex
	config map[int64]Config
	rows   map[[2]int64]row
}

func (m *Memory) FeedConfig(_ context.Context, id int64) (Config, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.config[id]
	if !ok {
		c.Dated = true
	}
	return c, nil
}
func (m *Memory) SetFeedConfig(_ context.Context, id int64, c Config) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.config == nil {
		m.config = make(map[int64]Config)
	}
	c.Welcome = m.config[id].Welcome
	m.config[id] = c
	return nil
}
func (m *Memory) SetFeedWelcome(_ context.Context, id int64, welcome string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.config == nil {
		m.config = make(map[int64]Config)
	}
	c, ok := m.config[id]
	if !ok {
		c.Dated = true
	}
	c.Welcome = welcome
	m.config[id] = c
	return nil
}
func (m *Memory) FeedEntries(_ context.Context, id int64) ([]Entry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := []Entry{}
	for k, r := range m.rows {
		if k[0] == id && !r.hidden {
			list = append(list, r.Entry)
		}
	}
	slices.SortFunc(list, func(a, b Entry) int {
		if c := b.Updated.Compare(a.Updated); c != 0 {
			return c
		}
		return cmp.Compare(b.Peer, a.Peer)
	})
	return list[:min(512, len(list))], nil
}
func (m *Memory) FeedUnread(_ context.Context, id int64) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var n int64
	for k, r := range m.rows {
		if k[0] == id && !r.hidden {
			n += int64(r.Unreads)
		}
	}
	return n, nil
}
func (m *Memory) SetFeedEntry(_ context.Context, uid, peer int64, data string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rows == nil {
		m.rows = make(map[[2]int64]row)
	}
	now := time.Now().UTC()
	for _, key := range [][2]int64{{uid, peer}, {peer, uid}} {
		r, exists := m.rows[key]
		if !exists || key[0] == peer {
			r.Unreads++
		}
		r.Peer, r.Data, r.Updated, r.hidden = key[1], data, now, false
		m.rows[key] = r
	}
	return nil
}
func (m *Memory) MarkFeedEntry(_ context.Context, uid, peer int64, hide bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := [2]int64{uid, peer}
	if r, ok := m.rows[k]; ok {
		if hide {
			r.hidden = true
		} else {
			r.Unreads = 0
		}
		m.rows[k] = r
	}
	return nil
}
