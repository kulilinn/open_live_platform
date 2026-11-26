package live

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"math"
	"slices"
	"sync"
	"time"
)

var (
	ErrRoomNotFound        = errors.New("room not found")
	ErrRoomClosed          = errors.New("room is closed")
	ErrForbidden           = errors.New("room permission denied")
	ErrNotMember           = errors.New("user is not a room member")
	ErrMuted               = errors.New("user is muted")
	ErrKicked              = errors.New("user was kicked from the room")
	ErrInvalidRoomPassword = errors.New("invalid room password")
	ErrRoomPasswordLocked  = errors.New("room password attempts exhausted")
	ErrRoomAlreadyOnline   = errors.New("room is already online")
	ErrViewerNIDExhausted  = errors.New("room RTC IDs exhausted")
)

type Room struct {
	ID                  int64       `json:"id"`
	OwnerID             int64       `json:"owner_id"`
	Title               string      `json:"title"`
	Online              bool        `json:"online"`
	MemberCount         int         `json:"member_count"`
	IsLock              bool        `json:"isLock"`
	Session             LiveSession `json:"-"`
	ViewerNID           uint32      `json:"-"` // RTC ID of the user in a JoinRoom result.
	TranslationLanguage string      `json:"-"`
}

type JoinedRoom struct {
	Room
	Members      []int64
	AnchorJoined bool
}

type RoomSettings struct {
	Title, Password, TranslationLanguage *string
}

func (r *Room) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	read := func(name string, target any) error {
		if value, ok := fields[name]; ok {
			return json.Unmarshal(value, target)
		}
		return nil
	}
	if err := read("id", &r.ID); err != nil {
		return err
	}
	if err := read("owner_id", &r.OwnerID); err != nil {
		return err
	}
	if err := read("title", &r.Title); err != nil {
		return err
	}
	if err := read("member_count", &r.MemberCount); err != nil {
		return err
	}
	if err := read("isLock", &r.IsLock); err != nil {
		return err
	}
	if value, ok := fields["online"]; ok {
		if err := json.Unmarshal(value, &r.Online); err != nil {
			var status int
			if err := json.Unmarshal(value, &status); err != nil {
				return err
			}
			r.Online = status != 0
		}
	}
	return nil
}

// RoomFilter selects rooms by online status and requests a one-based page.
// A nil Online matches all rooms. A zero PageSize requests only the matching count.
type RoomFilter struct {
	Online         *bool
	Page, PageSize int64
}

// Window computes the offset, length, and page count for total matching rooms.
// In count-only mode it returns the matching count as pages.
func (f RoomFilter) Window(total int64) (offset, limit, pages int64) {
	if f.PageSize == 0 || total == 0 {
		return 0, 0, total
	}
	pages = 1 + (total-1)/f.PageSize
	if f.Page < 1 || f.Page > pages {
		return 0, 0, pages
	}
	offset = (f.Page - 1) * f.PageSize
	return offset, min(f.PageSize, total-offset), pages
}

type Event struct {
	Type      string    `json:"type"`
	RoomID    int64     `json:"room_id"`
	UserID    int64     `json:"user_id"`
	TargetID  int64     `json:"target_id,omitempty"`
	Text      string    `json:"text,omitempty"`
	Payload   string    `json:"-"`
	MessageID int64     `json:"-"`
	CreatedAt time.Time `json:"-"`
}

const EventStreamCapacity = 33 // 32 pending events and one slot for the final close event.

// Enqueue keeps the last slot available for close on a subscription's buffered channel.
func (event Event) Enqueue(queue chan<- Event) bool {
	if event.Type != "close" && len(queue) >= cap(queue)-1 {
		return false
	}
	select {
	case queue <- event:
		return true
	default:
		return false
	}
}

// Subscription binds a room's event stream to the broadcast active at authorization.
type Subscription struct {
	Events      <-chan Event
	LiveID      int64
	Unsubscribe func()
}

type RoomStore interface {
	Start(ctx context.Context, ownerID int64, title, password string) (Room, error)
	StartWithLanguage(ctx context.Context, ownerID int64, title, password string, language *string) (Room, error)
	UpdateSettings(ctx context.Context, ownerID int64, settings RoomSettings) (Room, error)
	Close(ctx context.Context, roomID, actorID int64) error
	Info(ctx context.Context, roomID int64) (Room, error)
	Rooms(ctx context.Context) ([]Room, error)
	ListRooms(ctx context.Context, filter RoomFilter) ([]Room, int64, error)
	Join(ctx context.Context, roomID, userID int64, password string) error
	JoinRoom(ctx context.Context, roomID, userID int64, password string) (JoinedRoom, error)
	// RTCNID returns the RTC ID for the owner or a joined member of the active broadcast.
	RTCNID(ctx context.Context, roomID, userID int64) (uint32, error)
	Leave(ctx context.Context, roomID, userID int64) error
	Members(ctx context.Context, roomID int64) ([]int64, error)
	Admins(ctx context.Context, roomID int64) ([]int64, error)
	Kicked(ctx context.Context, roomID, userID int64) (bool, error)
	Muted(ctx context.Context, roomID, userID int64) (bool, error)
	MutedUsers(ctx context.Context, roomID int64) ([]int64, error)
	SetAdmin(ctx context.Context, roomID, actorID, targetID int64, enabled bool) error
	Mute(ctx context.Context, roomID, actorID, targetID int64, duration time.Duration) error
	Kick(ctx context.Context, roomID, actorID, targetID int64) error
	Send(ctx context.Context, roomID, userID int64, text string) error
	SendChat(ctx context.Context, msg ChatMessage) (int64, error)
	ChatHistory(ctx context.Context, roomID, userID int64, page, size uint64) ([]ChatMessage, error)
	Subscribe(ctx context.Context, roomID, userID int64) (<-chan Event, func(), error)
	SubscribeLive(ctx context.Context, roomID, userID int64) (Subscription, error)
}

type roomState struct {
	Room
	members          map[int64]struct{}
	anchorJoined     bool
	viewerNIDs       map[int64]uint32
	lastViewerNID    uint32 // Retained across broadcasts on the same channel.
	admins           map[int64]struct{}
	muted            map[int64]time.Time
	kicked           map[int64]struct{}
	subscribers      map[chan Event]int64
	passwordHash     []byte
	passwordAttempts map[int64]int
	history          []ChatMessage
}

type Store struct {
	mu            sync.Mutex
	rooms         map[int64]*roomState
	lastMessageID int64
	nextLiveID    int64
}

var _ RoomStore = (*Store)(nil)

func NewStore() *Store {
	return &Store{rooms: make(map[int64]*roomState), nextLiveID: 1 << 32}
}

// Start opens the creator's room. A new broadcast uses the same room ID.
func (s *Store) Start(ctx context.Context, ownerID int64, title, password string) (Room, error) {
	return s.StartWithLanguage(ctx, ownerID, title, password, nil)
}

func (s *Store) StartWithLanguage(ctx context.Context, ownerID int64, title, password string, language *string) (Room, error) {
	if err := ctx.Err(); err != nil {
		return Room{}, err
	}
	passwordHash, err := HashRoomPassword(password)
	if err != nil {
		return Room{}, err
	}

	s.mu.Lock()
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return Room{}, err
	}
	previous := s.rooms[ownerID]
	if previous != nil && previous.Online {
		info, hash := previous.snapshot(), previous.passwordHash
		s.mu.Unlock()
		if password != "" && (!info.IsLock || !RoomPasswordMatches(hash, password)) {
			return Room{}, ErrRoomAlreadyOnline
		}
		return info, nil
	}
	defer s.mu.Unlock()
	lastViewerNID := DefaultAnchorNID
	var translationLanguage string
	if previous != nil {
		lastViewerNID = previous.lastViewerNID
		translationLanguage = previous.TranslationLanguage
	}
	if language != nil {
		translationLanguage = *language
	}
	room := &roomState{
		Room: Room{
			ID:                  ownerID,
			OwnerID:             ownerID,
			Title:               title,
			Online:              true,
			IsLock:              len(passwordHash) != 0,
			TranslationLanguage: translationLanguage,
			Session: LiveSession{
				LiveID: s.nextLiveID, AnchorNID: DefaultAnchorNID, StartedAt: time.Now().UTC(),
			},
		},
		members:          make(map[int64]struct{}),
		viewerNIDs:       make(map[int64]uint32),
		lastViewerNID:    lastViewerNID,
		admins:           make(map[int64]struct{}),
		muted:            make(map[int64]time.Time),
		kicked:           make(map[int64]struct{}),
		subscribers:      make(map[chan Event]int64),
		passwordHash:     passwordHash,
		passwordAttempts: make(map[int64]int),
	}
	s.nextLiveID++
	s.rooms[room.ID] = room
	return room.snapshot(), nil
}

func (s *Store) UpdateSettings(ctx context.Context, ownerID int64, settings RoomSettings) (Room, error) {
	if err := ctx.Err(); err != nil {
		return Room{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	room, err := s.onlineRoom(ownerID)
	if err != nil {
		return Room{}, err
	}
	var hash []byte
	password := settings.Password
	if password != nil && *password != "" {
		s.mu.Unlock()
		hash, err = HashRoomPassword(*password)
		s.mu.Lock()
		if err != nil {
			return Room{}, err
		}
		if s.rooms[ownerID] != room || !room.Online {
			return Room{}, ErrRoomClosed
		}
	}
	if err := ctx.Err(); err != nil {
		return Room{}, err
	}
	if settings.Title != nil {
		room.Title = *settings.Title
	}
	if settings.TranslationLanguage != nil {
		room.TranslationLanguage = *settings.TranslationLanguage
	}
	if password != nil {
		room.passwordHash = hash
		room.IsLock = len(hash) != 0
		if !room.IsLock {
			clear(room.passwordAttempts)
		}
	}
	return room.snapshot(), nil
}

func (s *Store) Close(ctx context.Context, roomID, actorID int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	room := s.rooms[roomID]
	if room == nil {
		return ErrRoomNotFound
	}
	if err := room.checkCommandScope(ctx); err != nil {
		return err
	}
	if actorID != room.OwnerID {
		return ErrForbidden
	}
	room.end()
	return nil
}

func (s *Store) Info(ctx context.Context, roomID int64) (Room, error) {
	if err := ctx.Err(); err != nil {
		return Room{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	room := s.rooms[roomID]
	if room == nil {
		return Room{}, ErrRoomNotFound
	}
	return room.snapshot(), nil
}

func (s *Store) Rooms(ctx context.Context) ([]Room, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	rooms := make([]Room, 0, len(s.rooms))
	for _, id := range slices.Sorted(maps.Keys(s.rooms)) {
		rooms = append(rooms, s.rooms[id].snapshot())
	}
	return rooms, nil
}

func (s *Store) ListRooms(ctx context.Context, filter RoomFilter) ([]Room, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var matched []*roomState
	var total int64
	for _, room := range s.rooms {
		if filter.Online != nil && room.Online != *filter.Online {
			continue
		}
		total++
		if filter.PageSize > 0 {
			matched = append(matched, room)
		}
	}
	offset, limit, pages := filter.Window(total)
	if limit == 0 {
		return []Room{}, pages, nil
	}
	slices.SortFunc(matched, func(a, b *roomState) int {
		if a.Online == b.Online {
			return cmp.Compare(a.ID, b.ID)
		}
		if a.Online {
			return -1
		}
		return 1
	})
	rooms := make([]Room, 0, limit)
	for _, room := range matched[offset : offset+limit] {
		rooms = append(rooms, room.snapshot())
	}
	return rooms, pages, nil
}

func (s *Store) Join(ctx context.Context, roomID, userID int64, password string) error {
	_, err := s.joinRoom(ctx, roomID, userID, password, false)
	return err
}

// JoinRoom joins the user and returns the room snapshot of that broadcast.
func (s *Store) JoinRoom(ctx context.Context, roomID, userID int64, password string) (JoinedRoom, error) {
	return s.joinRoom(ctx, roomID, userID, password, true)
}

func (s *Store) joinRoom(ctx context.Context, roomID, userID int64, password string, includeMembers bool) (JoinedRoom, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var checkedRoom *roomState
	var checkedHash []byte
	var passwordMatches bool
	for {
		if err := ctx.Err(); err != nil {
			return JoinedRoom{}, err
		}
		room, err := s.onlineRoom(roomID)
		if err != nil {
			return JoinedRoom{}, err
		}
		if _, kicked := room.kicked[userID]; kicked {
			return JoinedRoom{}, ErrKicked
		}
		checkPassword := room.IsLock && userID != room.OwnerID
		if checkPassword {
			if room.passwordAttempts[userID] >= MaxRoomPasswordAttempts {
				return JoinedRoom{}, ErrRoomPasswordLocked
			}
			if checkedRoom != room || !bytes.Equal(checkedHash, room.passwordHash) {
				checkedRoom, checkedHash = room, room.passwordHash
				s.mu.Unlock()
				passwordMatches = RoomPasswordMatches(checkedHash, password)
				s.mu.Lock()
				// Recheck the broadcast, password, and viewer state after verification.
				continue
			}
			if !passwordMatches {
				room.passwordAttempts[userID]++
				return JoinedRoom{}, ErrInvalidRoomPassword
			}
		}
		nid, err := room.viewerNID(userID)
		if err != nil {
			return JoinedRoom{}, err
		}
		if checkPassword {
			delete(room.passwordAttempts, userID)
		}
		if userID == room.OwnerID {
			room.anchorJoined = true
		} else if !room.hasMember(userID) {
			room.members[userID] = struct{}{}
			room.publish(Event{Type: "join", UserID: userID})
		}
		info := JoinedRoom{Room: room.snapshot(), AnchorJoined: room.anchorJoined}
		info.ViewerNID = nid
		if includeMembers {
			info.Members = sortedIDs(room.members)
		}
		return info, nil
	}
}

// RTCNID returns the RTC ID for the owner or a joined member of the active broadcast.
func (s *Store) RTCNID(ctx context.Context, roomID, userID int64) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return 0, err
	}
	room, err := s.onlineRoom(roomID)
	if err != nil {
		return 0, err
	}
	if !room.hasMember(userID) {
		return 0, ErrNotMember
	}
	return room.viewerNID(userID)
}

func (s *Store) Leave(ctx context.Context, roomID, userID int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	room := s.rooms[roomID]
	if room == nil {
		return ErrRoomNotFound
	}
	if userID == room.OwnerID {
		room.end()
	} else if room.hasMember(userID) {
		delete(room.members, userID)
		room.publish(Event{Type: "leave", UserID: userID})
		room.disconnect(userID)
	}
	return nil
}

func (s *Store) Members(ctx context.Context, roomID int64) ([]int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	room := s.rooms[roomID]
	if room == nil {
		return nil, ErrRoomNotFound
	}
	return sortedIDs(room.members), nil
}

func (s *Store) Admins(ctx context.Context, roomID int64) ([]int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	room := s.rooms[roomID]
	if room == nil {
		return nil, ErrRoomNotFound
	}
	return sortedIDs(room.admins), nil
}

func (s *Store) Kicked(ctx context.Context, roomID, userID int64) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	room := s.rooms[roomID]
	if room == nil {
		return false, nil
	}
	_, kicked := room.kicked[userID]
	return kicked, nil
}

func (s *Store) Muted(ctx context.Context, roomID, userID int64) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	room := s.rooms[roomID]
	if room == nil {
		return false, nil
	}
	return time.Now().Before(room.muted[userID]), nil
}

func (s *Store) MutedUsers(ctx context.Context, roomID int64) ([]int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	room := s.rooms[roomID]
	if room == nil {
		return nil, ErrRoomNotFound
	}
	now := time.Now()
	ids := make([]int64, 0, len(room.muted))
	for id, until := range room.muted {
		if now.Before(until) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids, nil
}

func (s *Store) SetAdmin(ctx context.Context, roomID, actorID, targetID int64, enabled bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	room, err := s.onlineRoom(roomID)
	if err != nil {
		return err
	}
	if actorID != room.OwnerID {
		return ErrForbidden
	}
	if enabled {
		room.admins[targetID] = struct{}{}
	} else {
		delete(room.admins, targetID)
	}
	return nil
}

// Mute sets the mute duration. A zero duration clears it.
func (s *Store) Mute(ctx context.Context, roomID, actorID, targetID int64, duration time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	room, err := s.commandRoom(ctx, roomID)
	if err != nil {
		return err
	}
	if !room.canModerate(actorID, targetID) {
		return ErrForbidden
	}
	if duration == 0 {
		delete(room.muted, targetID)
	} else {
		room.muted[targetID] = time.Now().Add(duration)
	}
	return nil
}

func (s *Store) Kick(ctx context.Context, roomID, actorID, targetID int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	room, err := s.commandRoom(ctx, roomID)
	if err != nil {
		return err
	}
	if !room.canModerate(actorID, targetID) {
		return ErrForbidden
	}
	if !room.hasMember(targetID) {
		return ErrNotMember
	}
	room.kicked[targetID] = struct{}{}
	delete(room.members, targetID)
	room.publish(Event{Type: "kick", UserID: actorID, TargetID: targetID})
	room.disconnect(targetID)
	return nil
}

func (s *Store) Send(ctx context.Context, roomID, userID int64, text string) error {
	_, err := s.SendChat(ctx, ChatMessage{RoomID: roomID, UserID: userID, Text: text})
	return err
}

func (s *Store) SendChat(ctx context.Context, msg ChatMessage) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	room, err := s.commandRoom(ctx, msg.RoomID)
	if err != nil {
		return 0, err
	}
	if !room.hasMember(msg.UserID) {
		return 0, ErrNotMember
	}
	now := time.Now()
	if now.Before(room.muted[msg.UserID]) {
		return 0, ErrMuted
	}

	s.lastMessageID++
	msg.ID, msg.CreatedAt = s.lastMessageID, now
	if len(room.history) > 0 && now.Sub(room.history[0].CreatedAt) >= historyTTL {
		room.history = nil
	}
	if len(room.history) < HistoryLimit {
		room.history = append(room.history, ChatMessage{})
	}
	copy(room.history[1:], room.history)
	room.history[0] = msg
	room.publish(Event{
		Type: "chat", UserID: msg.UserID, Text: msg.Text, Payload: msg.Payload,
		MessageID: msg.ID, CreatedAt: msg.CreatedAt,
	})
	return msg.ID, nil
}

func (s *Store) ChatHistory(ctx context.Context, roomID, userID int64, page, size uint64) ([]ChatMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	room, err := s.onlineRoom(roomID)
	if err != nil {
		return nil, err
	}
	if !room.hasMember(userID) {
		return nil, ErrNotMember
	}
	return HistoryPage(room.history, page, size, time.Now()), nil
}

// Subscribe receives live events for a joined user or the room owner.
func (s *Store) Subscribe(ctx context.Context, roomID, userID int64) (<-chan Event, func(), error) {
	sub, err := s.SubscribeLive(ctx, roomID, userID)
	return sub.Events, sub.Unsubscribe, err
}

func (s *Store) SubscribeLive(ctx context.Context, roomID, userID int64) (Subscription, error) {
	if err := ctx.Err(); err != nil {
		return Subscription{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	room, err := s.onlineRoom(roomID)
	if err != nil {
		return Subscription{}, err
	}
	if !room.hasMember(userID) {
		return Subscription{}, ErrNotMember
	}
	events := make(chan Event, EventStreamCapacity)
	room.subscribers[events] = userID
	unsubscribe := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		room.unsubscribe(events)
	}
	return Subscription{Events: events, LiveID: room.Session.LiveID, Unsubscribe: unsubscribe}, nil
}

func (s *Store) commandRoom(ctx context.Context, id int64) (*roomState, error) {
	room, err := s.onlineRoom(id)
	if err != nil {
		return nil, err
	}
	if err := room.checkCommandScope(ctx); err != nil {
		return nil, err
	}
	return room, nil
}

func (r *roomState) checkCommandScope(ctx context.Context) error {
	roomID, liveID, scoped := RoomCommandScope(ctx)
	if scoped && (r.ID != roomID || !r.Online || r.Session.LiveID != liveID) {
		return ErrRoomClosed
	}
	return nil
}

func (s *Store) onlineRoom(id int64) (*roomState, error) {
	room := s.rooms[id]
	if room == nil {
		return nil, ErrRoomNotFound
	}
	if !room.Online {
		return nil, ErrRoomClosed
	}
	return room, nil
}

func (r *roomState) snapshot() Room {
	info := r.Room
	info.MemberCount = len(r.members)
	return info
}

func (r *roomState) viewerNID(userID int64) (uint32, error) {
	if userID == r.OwnerID {
		return r.Session.AnchorNID, nil
	}
	if nid, assigned := r.viewerNIDs[userID]; assigned {
		return nid, nil
	}
	if r.lastViewerNID == math.MaxUint32 {
		return 0, ErrViewerNIDExhausted
	}
	r.lastViewerNID++
	r.viewerNIDs[userID] = r.lastViewerNID
	return r.lastViewerNID, nil
}

func (r *roomState) hasMember(userID int64) bool {
	_, joined := r.members[userID]
	return joined || userID == r.OwnerID
}

func (r *roomState) canModerate(actorID, targetID int64) bool {
	_, admin := r.admins[actorID]
	return targetID != r.OwnerID && (actorID == r.OwnerID || admin)
}

func (r *roomState) end() {
	if !r.Online {
		return
	}
	r.Online = false
	r.Session.EndedAt = time.Now().UTC()
	r.IsLock = false
	r.passwordHash = nil
	r.passwordAttempts = nil
	r.history = nil
	clear(r.members)
	r.anchorJoined = false
	r.viewerNIDs = nil
	r.publish(Event{Type: "close", UserID: r.OwnerID, CreatedAt: r.Session.EndedAt})
	for events := range r.subscribers {
		r.unsubscribe(events)
	}
}

func (r *roomState) disconnect(userID int64) {
	for events, subscriberID := range r.subscribers {
		if subscriberID == userID {
			r.unsubscribe(events)
		}
	}
}

func (r *roomState) unsubscribe(events chan Event) {
	if _, subscribed := r.subscribers[events]; subscribed {
		delete(r.subscribers, events)
		close(events)
	}
}

func (r *roomState) publish(event Event) {
	event.RoomID = r.ID
	for events := range r.subscribers {
		if !event.Enqueue(events) {
			r.unsubscribe(events)
		}
	}
}

func sortedIDs(ids map[int64]struct{}) []int64 {
	result := slices.AppendSeq(make([]int64, 0, len(ids)), maps.Keys(ids))
	slices.Sort(result)
	return result
}
