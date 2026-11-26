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

const (
	anchorLeaseDuration     = 30 * time.Second
	anchorHeartbeatInterval = 5 * time.Second
	anchorHeartbeatTimeout  = 3 * time.Second
)

type anchorPresence struct {
	mu      sync.RWMutex
	closing bool
	active  sync.Map // anchorConnection keys
}

func (s *Store) AnchorConnected(ctx context.Context, roomID, liveID int64, connectionID string) error {
	s.anchors.mu.RLock()
	defer s.anchors.mu.RUnlock()
	if s.anchors.closing || !s.eventReady.Load() {
		return errRoomEventsUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, anchorHeartbeatTimeout)
	defer cancel()
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
		_, err = r.tx.Exec(ctx, `INSERT INTO room_anchor_connections(room_id,live_id,connection_id,lease_until)
			VALUES($1,$2,$3,statement_timestamp()+make_interval(secs => $4))
			ON CONFLICT (room_id,live_id,connection_id) DO UPDATE SET lease_until=EXCLUDED.lease_until`,
			roomID, liveID, connectionID, anchorLeaseDuration.Seconds())
		if err != nil || !away {
			return err
		}
		return s.setAnchorAway(r, owner, false)
	})
	if err == nil {
		s.anchors.active.Store(anchorConnection{roomID, liveID, connectionID}, struct{}{})
	}
	return err
}

func (s *Store) AnchorDisconnected(ctx context.Context, roomID, liveID int64, connectionID string) error {
	// A closed socket must stop renewing even if its database cleanup fails.
	s.anchors.active.Delete(anchorConnection{roomID, liveID, connectionID})
	err := s.change(ctx, roomID, func(r *roomChange) error {
		tag, err := r.tx.Exec(ctx, `DELETE FROM room_anchor_connections c USING rooms r
			WHERE c.room_id=$1 AND c.live_id=$2 AND c.connection_id=$3
				AND r.id=c.room_id AND r.live_id=c.live_id AND r.online`, roomID, liveID, connectionID)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		return s.markAnchorAwayIfEmpty(r, liveID)
	})
	if errors.Is(err, live.ErrRoomNotFound) || errors.Is(err, live.ErrRoomClosed) {
		err = nil
	}
	return err
}

func (s *Store) renewAnchorLeases(ctx context.Context, conn *pgx.Conn) error {
	var roomIDs, liveIDs []int64
	var connectionIDs []string
	s.anchors.active.Range(func(key, _ any) bool {
		connection := key.(anchorConnection)
		roomIDs = append(roomIDs, connection.roomID)
		liveIDs = append(liveIDs, connection.liveID)
		connectionIDs = append(connectionIDs, connection.connectionID)
		return true
	})
	if len(connectionIDs) == 0 {
		return conn.Ping(ctx)
	}
	_, err := conn.Exec(ctx, `WITH available AS (
		SELECT c.room_id,c.live_id,c.connection_id FROM room_anchor_connections c
		JOIN unnest($1::bigint[],$2::bigint[],$3::text[]) a(room_id,live_id,connection_id)
			USING (room_id,live_id,connection_id)
		WHERE c.lease_until>clock_timestamp() FOR UPDATE OF c SKIP LOCKED
	)
	UPDATE room_anchor_connections c
		SET lease_until=clock_timestamp()+make_interval(secs => $4)
		FROM available a
		WHERE c.room_id=a.room_id AND c.live_id=a.live_id AND c.connection_id=a.connection_id
			AND c.lease_until>clock_timestamp()`, roomIDs, liveIDs, connectionIDs, anchorLeaseDuration.Seconds())
	return err
}

func (s *Store) stopAnchorLeases() {
	// A registration already in progress may still store its connection.
	// Fence those registrations before discarding the failed listener's leases.
	s.anchors.mu.Lock()
	s.anchors.active.Clear()
	s.anchors.mu.Unlock()
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

func (s *Store) setAnchorAway(r *roomChange, owner int64, away bool) error {
	var changedAt time.Time
	err := r.tx.QueryRow(r.ctx, `UPDATE rooms SET anchor_away=$2,updated_at=statement_timestamp(),
		anchor_away_until=CASE WHEN $2 THEN statement_timestamp()+make_interval(secs => $3) END
		WHERE id=$1 AND anchor_away<>$2 RETURNING updated_at`, r.id, away, s.anchorAwayTimeout.Seconds()).Scan(&changedAt)
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

func (s *Store) markAnchorAwayIfEmpty(r *roomChange, liveID int64) error {
	var owner int64
	err := r.tx.QueryRow(r.ctx, `SELECT owner_id FROM rooms WHERE id=$1 AND live_id=$2 AND online
		AND NOT EXISTS (SELECT 1 FROM room_anchor_connections WHERE room_id=$1 AND live_id=$2)`, r.id, liveID).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.setAnchorAway(r, owner, true)
}

func (s *Store) expireAnchorLeases(ctx context.Context) error {
	type candidate struct{ RoomID, LiveID int64 }
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT room_id,live_id FROM room_anchor_connections
		WHERE lease_until<=statement_timestamp()`)
	if err != nil {
		return err
	}
	candidates, err := pgx.CollectRows(rows, pgx.RowToStructByPos[candidate])
	if err != nil {
		return err
	}
	var sweepErr error
	for _, room := range candidates {
		err := s.change(ctx, room.RoomID, func(r *roomChange) error {
			// A heartbeat can renew a row after the scan. DELETE must recheck
			// its lease, and the room lock prevents concurrent registrations.
			tag, err := r.tx.Exec(ctx, `DELETE FROM room_anchor_connections
				WHERE room_id=$1 AND live_id=$2 AND lease_until<=statement_timestamp()`, room.RoomID, room.LiveID)
			if err != nil || tag.RowsAffected() == 0 {
				return err
			}
			return s.markAnchorAwayIfEmpty(r, room.LiveID)
		})
		sweepErr = errors.Join(sweepErr, err)
		if ctx.Err() != nil {
			break
		}
	}
	return sweepErr
}

func (s *Store) CloseExpiredAnchorRooms(ctx context.Context, now time.Time) error {
	sweepErr := s.expireAnchorLeases(ctx)
	if ctx.Err() != nil {
		return sweepErr
	}
	type candidate struct {
		RoomID, LiveID int64
		Until          time.Time
	}
	rows, err := s.pool.Query(ctx, `SELECT id,live_id,anchor_away_until FROM rooms
		WHERE online AND anchor_away AND anchor_away_until<=LEAST($1,statement_timestamp())
		ORDER BY anchor_away_until`, now)
	if err != nil {
		return errors.Join(sweepErr, err)
	}
	candidates, err := pgx.CollectRows(rows, pgx.RowToStructByPos[candidate])
	if err != nil {
		return errors.Join(sweepErr, err)
	}
	for _, room := range candidates {
		err := s.change(ctx, room.RoomID, func(r *roomChange) error {
			// Recheck the session and deadline after acquiring the room lock.
			var owner int64
			err := r.tx.QueryRow(ctx, `SELECT owner_id FROM rooms
				WHERE id=$1 AND online AND anchor_away AND live_id=$2
					AND anchor_away_until=$3 AND anchor_away_until<=LEAST($4,statement_timestamp())`,
				room.RoomID, room.LiveID, room.Until, now).Scan(&owner)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			return r.close(owner)
		})
		sweepErr = errors.Join(sweepErr, err)
		if ctx.Err() != nil {
			break
		}
	}
	return sweepErr
}
