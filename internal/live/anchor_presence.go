package live

import (
	"context"
	"time"
)

// AnchorConnected tracks an authenticated H5 connection in the broadcast it subscribed to.
func (s *Store) AnchorConnected(ctx context.Context, roomID, liveID int64, connectionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	room, err := s.onlineRoom(roomID)
	if err != nil {
		return err
	}
	if room.Session.LiveID != liveID {
		return ErrRoomClosed
	}
	if room.anchorConnections == nil {
		room.anchorConnections = make(map[string]struct{})
	}
	room.anchorConnections[connectionID] = struct{}{}
	room.setAnchorAwayUntil(time.Time{})
	return nil
}

// AnchorDisconnected ignores cleanup from a connection belonging to an older broadcast.
func (s *Store) AnchorDisconnected(ctx context.Context, roomID, liveID int64, connectionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	room := s.rooms[roomID]
	if room == nil || !room.Online || room.Session.LiveID != liveID {
		return nil
	}
	if _, connected := room.anchorConnections[connectionID]; connected {
		delete(room.anchorConnections, connectionID)
		if len(room.anchorConnections) == 0 {
			room.setAnchorAwayUntil(time.Now().UTC().Add(s.anchorAwayTimeout))
		}
	}
	return nil
}

func (s *Store) CloseExpiredAnchorRooms(ctx context.Context, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, room := range s.rooms {
		if room.Online && room.AnchorAway && !room.AnchorAwayUntil.IsZero() && !room.AnchorAwayUntil.After(now) {
			room.end()
		}
	}
	return nil
}

func (r *roomState) setAnchorAwayUntil(until time.Time) {
	away := !until.IsZero()
	if r.AnchorAway == away {
		return
	}
	r.AnchorAway = away
	r.AnchorAwayUntil = until
	r.publish(Event{
		Type: "room_status", UserID: r.OwnerID, RoomStatus: r.Status(), CreatedAt: time.Now().UTC(),
	})
}
