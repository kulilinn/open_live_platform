package live

import (
	"sync"
	"time"
)

const DefaultAnchorNID int64 = 200000001

type LiveSession struct {
	RoomID    int64     `json:"room_id"`
	LiveID    int64     `json:"live_id"`
	AnchorNID int64     `json:"anchor_nid"`
	StartedAt time.Time `json:"started_at"`
}

type LiveSessionStore struct {
	mu       sync.Mutex
	nextID   int64
	sessions map[int64]LiveSession
}

func NewLiveSessionStore() *LiveSessionStore {
	return &LiveSessionStore{sessions: make(map[int64]LiveSession)}
}

func (s *LiveSessionStore) Start(roomID, anchorNID int64, startedAt time.Time) LiveSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	if anchorNID == 0 {
		anchorNID = DefaultAnchorNID
	}
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	session := LiveSession{
		RoomID: roomID, LiveID: s.nextID, AnchorNID: anchorNID, StartedAt: startedAt,
	}
	s.sessions[roomID] = session
	return session
}

func (s *LiveSessionStore) Get(roomID int64) (LiveSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[roomID]
	return session, ok
}

func (s *LiveSessionStore) End(roomID int64) (LiveSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[roomID]
	if ok {
		delete(s.sessions, roomID)
	}
	return session, ok
}
