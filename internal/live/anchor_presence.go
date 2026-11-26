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
	room.setAnchorAway(false)
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
			room.setAnchorAway(true)
		}
	}
	return nil
}

func (r *roomState) setAnchorAway(away bool) {
	if r.AnchorAway == away {
		return
	}
	r.AnchorAway = away
	r.publish(Event{
		Type: "room_status", UserID: r.OwnerID, RoomStatus: r.Status(), CreatedAt: time.Now().UTC(),
	})
}
