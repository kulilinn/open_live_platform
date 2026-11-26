package postgres

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/live"
)

type Store struct {
	bots                *roomBots
	pool                *pgxpool.Pool
	anchorAwayTimeout   time.Duration
	eventChannel        string
	eventReady          atomic.Bool
	stopEvents          context.CancelFunc
	eventsDone          chan struct{}
	shutdown            sync.Once
	anchors             anchorPresence
	sessionRevocations  auth.SessionRevocations
	relayDeviceReceiver atomic.Pointer[relayDeviceReceiver]
	// Independent rooms can publish in parallel. Each shard orders local
	// subscriptions and delivery with the commits that change their room.
	streams [32]streams
}

type streams struct {
	sync.Mutex
	rooms map[int64]map[chan live.Event]subscriber
}

type subscriber struct {
	userID       int64
	marker       string
	translations bool
}

var _ auth.JWTBackend = (*Store)(nil)
var _ live.RoomStore = (*Store)(nil)

func Open(ctx context.Context, dsn string) (*Store, error) {
	return OpenWithAnchorTimeout(ctx, dsn, live.DefaultAnchorAwayTimeout)
}

func OpenWithAnchorTimeout(ctx context.Context, dsn string, timeout time.Duration) (*Store, error) {
	if timeout <= 0 {
		timeout = live.DefaultAnchorAwayTimeout
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	s := &Store{pool: pool, anchorAwayTimeout: timeout}
	if err := s.startRoomEvents(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Shutdown() {
	s.shutdown.Do(func() {
		s.closeAnchorPresence()
		s.stopEvents()
		<-s.eventsDone
		s.pool.Close()
	})
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
		if sub.marker != "" || ((event.Type == "translate_change" || event.Type == "gift") && !sub.translations) {
			continue
		}
		disconnect := event.Type == "close" || (event.Type == "kick" && sub.userID == event.TargetID) || (event.Type == "leave" && sub.userID == event.UserID)
		if !event.Enqueue(ch) || disconnect {
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
	// Reads after the advisory lock must see the committed state of the previous holder.
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1::bigint)", id); err != nil {
			return err
		}
		r = roomChange{ctx: ctx, tx: tx, id: id, bots: s.bots}
		if err := r.checkCommandScope(); err != nil {
			return err
		}
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
		stream.rooms[id][r.sub] = subscriber{userID: r.subUser, marker: r.subMarker, translations: r.subTranslations}
	}
	return nil
}

type roomChange struct {
	bots              *roomBots
	ctx               context.Context
	tx                pgx.Tx
	id                int64
	event             live.Event
	translationChange roomSignal
	sub               chan live.Event
	subUser           int64
	subMarker         string
	subTranslations   bool
}

func (r *roomChange) checkCommandScope() error {
	roomID, liveID, scoped := live.RoomCommandScope(r.ctx)
	if !scoped {
		return nil
	}
	if roomID != r.id {
		return live.ErrRoomClosed
	}
	var matches bool
	err := r.tx.QueryRow(r.ctx, "SELECT online AND live_id=$2 FROM rooms WHERE id=$1", r.id, liveID).Scan(&matches)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return live.ErrRoomNotFound
	case err != nil:
		return err
	case !matches:
		return live.ErrRoomClosed
	}
	return nil
}
