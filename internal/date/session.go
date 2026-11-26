package date

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"open_live_platform/internal/live"
	"open_live_platform/internal/stt"
)

const (
	AnchorNID        uint32 = 100001
	AudienceNID      uint32 = 100002
	InviteLifetime          = time.Minute
	CallCooldown            = 10 * time.Second
	ParticipantLease        = 10 * time.Second
)

var (
	ErrNotFound     = errors.New("data not found")
	ErrForbidden    = errors.New("forbidden")
	ErrClosed       = errors.New("RTC session closed")
	ErrSelf         = errors.New("cannot call yourself")
	ErrBusy         = errors.New("participant is busy")
	ErrNotListening = errors.New("anchor is not accepting calls")
	ErrCooldown     = errors.New("callin is temporarily unavailable")
	ErrNoInvitation = errors.New("fan did not call in")
)

type Host struct {
	Channel      int64
	Accepting    bool
	BaseSettings json.RawMessage
	Settings     json.RawMessage
	PricePerMin  int64
	InitialMins  int64
	Pending      *Invitation
	Cooldowns    map[int64]time.Time
}

type Invitation struct {
	UID         int64  `json:"uid,string"`
	NID         uint32 `json:"nid"`
	Avatar      string `json:"avatar"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
	Comment     string `json:"comment"`
	Timestamp   int64  `json:"timestamp"`
	Tips        int64  `json:"tips"`
	Level       int    `json:"lv"`
	Intimacy    int    `json:"intimacy"`
	Language    string `json:"lang"`
	DelayedTask int64  `json:"delayedTask,string"`
}

func (i *Invitation) Active(now time.Time) bool {
	return i != nil && now.Before(time.Unix(i.Timestamp, 0).Add(InviteLifetime))
}

type Session struct {
	Channel, AnchorID, AudienceID    int64
	StartedAt, EndedAt               time.Time
	AnchorSeen, AudienceSeen         time.Time
	AnchorJoined, AudienceJoined     bool
	AnchorLanguage, AudienceLanguage string
	PricePerMin, Points              int64
	Subtitles                        stt.Task
}

func (s Session) Includes(user int64) bool {
	return user != 0 && (s.AnchorID == user || s.AudienceID == user)
}

func (s Session) Active(now time.Time) bool {
	return s.Channel != 0 && s.EndedAt.IsZero() && now.Before(s.AnchorSeen.Add(ParticipantLease)) && now.Before(s.AudienceSeen.Add(ParticipantLease))
}

func (s Session) Participant(user int64) (nid uint32, peer int64, peerNID uint32) {
	if s.AnchorID == user {
		return AnchorNID, s.AudienceID, AudienceNID
	}
	return AudienceNID, s.AnchorID, AnchorNID
}

func (s Session) Seconds(now time.Time) int64 {
	if !s.EndedAt.IsZero() {
		now = s.EndedAt
	}
	return max(0, int64(now.Sub(s.StartedAt)/time.Second))
}

type Store interface {
	ChangeDateHost(context.Context, int64, func(*Host, *Session, time.Time) error) error
	DateHost(context.Context, int64) (Host, Session, error)
	DateSession(context.Context, int64) (Session, error)
	ActiveDateHosts(context.Context) ([]int64, error)
	DateSubtitles() stt.Store
}

func NewChannel() int64 {
	var raw [8]byte
	rand.Read(raw[:])
	return int64((binary.BigEndian.Uint64(raw[:]) & ((1 << 60) - 1)) + (1 << 60))
}

func End(host *Host, s *Session, now time.Time) {
	if s.Channel == 0 || !s.EndedAt.IsZero() {
		return
	}
	s.EndedAt = now
	if host.Channel == s.Channel {
		host.Channel = NewChannel()
	}
}

type Memory struct {
	mu       sync.Mutex
	hosts    map[int64]Host
	sessions map[int64]Session
	sttLocks [32]sync.Mutex
}

func NewMemory() *Memory {
	return &Memory{hosts: make(map[int64]Host), sessions: make(map[int64]Session)}
}

func (s *Memory) ChangeDateHost(ctx context.Context, id int64, update func(*Host, *Session, time.Time) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	host := cloneHost(s.hosts[id])
	session := s.sessions[host.Channel]
	before := session
	if err := update(&host, &session, time.Now().UTC()); err != nil {
		return err
	}
	if session.Channel != 0 && session.EndedAt.IsZero() && before.Channel != session.Channel {
		for _, other := range s.sessions {
			if other.EndedAt.IsZero() && (other.Channel == session.Channel || other.Includes(session.AnchorID) || other.Includes(session.AudienceID)) {
				return ErrBusy
			}
		}
	}
	s.hosts[id] = host
	if session.Channel != 0 {
		s.sessions[session.Channel] = session
	}
	return nil
}

func (s *Memory) DateHost(ctx context.Context, id int64) (Host, Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	host := cloneHost(s.hosts[id])
	return host, s.sessions[host.Channel], ctx.Err()
}

func (s *Memory) DateSession(ctx context.Context, channel int64) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, found := s.sessions[channel]
	if !found {
		return Session{}, ErrNotFound
	}
	return session, ctx.Err()
}

func (s *Memory) ActiveDateHosts(ctx context.Context) ([]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]int64, 0, len(s.hosts))
	for id, host := range s.hosts {
		if host.Pending != nil || s.sessions[host.Channel].Channel != 0 {
			ids = append(ids, id)
		}
	}
	return ids, ctx.Err()
}

func cloneHost(host Host) Host {
	if host.Pending != nil {
		pending := *host.Pending
		host.Pending = &pending
	}
	if host.Cooldowns != nil {
		cooldowns := make(map[int64]time.Time, len(host.Cooldowns))
		for id, until := range host.Cooldowns {
			cooldowns[id] = until
		}
		host.Cooldowns = cooldowns
	}
	return host
}

type dateSubtitles struct{ store *Memory }

func (s *Memory) DateSubtitles() stt.Store { return dateSubtitles{s} }

func (s dateSubtitles) ChangeSTTTask(ctx context.Context, channel int64, change func(stt.RoomState, *stt.Task) error) error {
	lock := &s.store.sttLocks[uint64(channel)%uint64(len(s.store.sttLocks))]
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	s.store.mu.Lock()
	session, ok := s.store.sessions[channel]
	s.store.mu.Unlock()
	if !ok {
		return live.ErrRoomNotFound
	}
	room := stt.RoomState{OwnerID: session.AnchorID, ParticipantID: session.AudienceID, LiveID: channel, Online: session.Active(time.Now())}
	if err := change(room, &session.Subtitles); err != nil {
		return err
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	current := s.store.sessions[channel]
	if session.Subtitles != current.Subtitles && session.Subtitles.RemoteID != "" && !current.Active(time.Now()) {
		return ErrClosed
	}
	current.Subtitles = session.Subtitles
	s.store.sessions[channel] = current
	return nil
}

func (s dateSubtitles) StaleSTTTaskRooms(ctx context.Context) ([]int64, error) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	var ids []int64
	for id, session := range s.store.sessions {
		if session.Subtitles.RemoteID != "" && !session.Active(time.Now()) {
			ids = append(ids, id)
		}
	}
	return ids, ctx.Err()
}
