package postgres

import (
	"context"
	_ "embed"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/live"
)

//go:embed migrations/001_core.sql
var schema string

//go:embed migrations/002_room_passwords.sql
var roomPasswordsSchema string

//go:embed migrations/003_room_chat_history.sql
var roomChatHistorySchema string

//go:embed migrations/004_user_language.sql
var userLanguageSchema string

//go:embed migrations/005_live_session.sql
var liveSessionSchema string

//go:embed migrations/006_auth_sessions.sql
var jwtSessionsSchema string

type Store struct {
	pool         *pgxpool.Pool
	eventChannel string
	eventReady   atomic.Bool
	stopEvents   context.CancelFunc
	eventsDone   chan struct{}
	// Independent rooms can publish in parallel. Each shard orders local
	// subscriptions and delivery with the commits that change their room.
	streams [32]streams
}

type streams struct {
	sync.Mutex
	rooms map[int64]map[chan live.Event]subscriber
}

type subscriber struct {
	userID int64
	marker string
}

var _ auth.JWTBackend = (*Store)(nil)
var _ live.RoomStore = (*Store)(nil)

func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	// Serialize schema creation and upgrades on this database.
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(733516, 1)"); err != nil {
			return err
		}
		for _, migration := range []string{schema, roomPasswordsSchema, roomChatHistorySchema, userLanguageSchema, liveSessionSchema, jwtSessionsSchema} {
			if _, err := tx.Exec(ctx, migration); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		pool.Close()
		return nil, err
	}
	s := &Store{pool: pool}
	if err := s.startRoomEvents(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Shutdown() {
	s.stopEvents()
	<-s.eventsDone
	s.pool.Close()
}

func (s *Store) closeSubscriptions() {
	for i := range s.streams {
		stream := &s.streams[i]
		stream.Lock()
		for _, subscribers := range stream.rooms {
			for ch := range subscribers {
				close(ch)
			}
		}
		clear(stream.rooms)
		stream.Unlock()
	}
}

func (s *Store) stream(id int64) *streams {
	return &s.streams[uint64(id)%uint64(len(s.streams))]
}

func (s *streams) publish(event live.Event) {
	listeners := s.rooms[event.RoomID]
	for ch, sub := range listeners {
		if sub.marker != "" {
			continue
		}
		disconnect := event.Type == "close" || (event.Type == "kick" && sub.userID == event.TargetID) || (event.Type == "leave" && sub.userID == event.UserID)
		select {
		case ch <- event:
		default:
			disconnect = true
		}
		if disconnect {
			delete(listeners, ch)
			close(ch)
		}
	}
	if len(listeners) == 0 {
		delete(s.rooms, event.RoomID)
	}
}

// Change serializes a room's SQL preconditions and mutations, including
// concurrent clients using the database from separate platform processes.
func (s *Store) change(ctx context.Context, id int64, fn func(*roomChange) error) error {
	stream := s.stream(id)
	stream.Lock()
	defer stream.Unlock()
	var r roomChange
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1::bigint)", id); err != nil {
			return err
		}
		r = roomChange{ctx: ctx, tx: tx, id: id}
		if err := fn(&r); err != nil {
			return err
		}
		return s.notifyRoomChange(&r)
	})
	if err != nil {
		return err
	}
	if r.sub != nil {
		if stream.rooms == nil {
			stream.rooms = make(map[int64]map[chan live.Event]subscriber)
		}
		if stream.rooms[id] == nil {
			stream.rooms[id] = make(map[chan live.Event]subscriber)
		}
		stream.rooms[id][r.sub] = subscriber{userID: r.subUser, marker: r.subMarker}
	}
	return nil
}

type roomChange struct {
	ctx       context.Context
	tx        pgx.Tx
	id        int64
	event     live.Event
	messageID int64
	sub       chan live.Event
	subUser   int64
	subMarker string
}
