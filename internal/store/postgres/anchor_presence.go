package postgres

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"open_live_platform/internal/live"
)

type anchorConnection struct {
	roomID, liveID int64
	connectionID   string
}

type anchorPresence struct {
	mu      sync.RWMutex
	closing bool
	active  sync.Map // anchorConnection keys
}

func (s *Store) AnchorConnected(ctx context.Context, roomID, liveID int64, connectionID string) error {
	s.anchors.mu.RLock()
	defer s.anchors.mu.RUnlock()
	if s.anchors.closing {
		return errRoomEventsUnavailable
	}
	err := s.change(ctx, roomID, func(r *roomChange) error {
		var owner, currentLiveID int64
		var online, away bool
		err := r.tx.QueryRow(ctx, `SELECT owner_id, live_id, online, anchor_away FROM rooms WHERE id=$1`, roomID).
			Scan(&owner, &currentLiveID, &online, &away)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return live.ErrRoomNotFound
		case err != nil:
			return err
		case !online || currentLiveID != liveID:
			return live.ErrRoomClosed
		}
		_, err = r.tx.Exec(ctx, `INSERT INTO room_anchor_connections(room_id,live_id,connection_id)
			VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, roomID, liveID, connectionID)
		if err != nil || !away {
			return err
		}
		return r.setAnchorAway(owner, false)
	})
	if err == nil {
		s.anchors.active.Store(anchorConnection{roomID, liveID, connectionID}, struct{}{})
	}
	return err
}

func (s *Store) AnchorDisconnected(ctx context.Context, roomID, liveID int64, connectionID string) error {
	err := s.change(ctx, roomID, func(r *roomChange) error {
		tag, err := r.tx.Exec(ctx, `DELETE FROM room_anchor_connections c USING rooms r
			WHERE c.room_id=$1 AND c.live_id=$2 AND c.connection_id=$3
				AND r.id=c.room_id AND r.live_id=c.live_id AND r.online`, roomID, liveID, connectionID)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		var owner int64
		var connected bool
		err = r.tx.QueryRow(ctx, `SELECT owner_id, EXISTS(
			SELECT 1 FROM room_anchor_connections WHERE room_id=$1 AND live_id=$2
		) FROM rooms WHERE id=$1`, roomID, liveID).Scan(&owner, &connected)
		if err != nil || connected {
			return err
		}
		return r.setAnchorAway(owner, true)
	})
	if errors.Is(err, live.ErrRoomNotFound) || errors.Is(err, live.ErrRoomClosed) {
		err = nil
	}
	if err == nil {
		s.anchors.active.Delete(anchorConnection{roomID, liveID, connectionID})
	}
	return err
}

func (s *Store) closeAnchorPresence() {
	// Stop new registrations before draining active connections. HTTP shutdown
	// leaves hijacked sockets open, so their normal cleanup can occur later.
	s.anchors.mu.Lock()
	s.anchors.closing = true
	s.anchors.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s.anchors.active.Range(func(key, _ any) bool {
		connection := key.(anchorConnection)
		if err := s.AnchorDisconnected(ctx, connection.roomID, connection.liveID, connection.connectionID); err != nil {
			log.Printf("disconnect anchor in room %d during PostgreSQL store shutdown: %v", connection.roomID, err)
		}
		return ctx.Err() == nil
	})
}

func (r *roomChange) setAnchorAway(owner int64, away bool) error {
	var changedAt time.Time
	err := r.tx.QueryRow(r.ctx, `UPDATE rooms SET anchor_away=$2,updated_at=statement_timestamp()
		WHERE id=$1 AND anchor_away<>$2 RETURNING updated_at`, r.id, away).Scan(&changedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	status := live.RoomOnline
	if away {
		status = live.RoomAnchorAway
	}
	r.event = live.Event{Type: "room_status", UserID: owner, CreatedAt: changedAt, RoomStatus: status}
	return nil
}
