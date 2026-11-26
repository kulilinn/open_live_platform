package live

import (
	"context"
	"time"
)

func (s *Store) SetRoomStatus(ctx context.Context, roomID, actor int64, status RoomStatus) (Room, error) {
	if err := ctx.Err(); err != nil {
		return Room{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	room := s.rooms[roomID]
	if room == nil {
		return Room{}, ErrRoomNotFound
	}
	if actor != room.OwnerID || status > RoomAnchorAway {
		return Room{}, ErrForbidden
	}
	if err := room.checkCommandScope(ctx); err != nil {
		return Room{}, err
	}
	if room.Limited(time.Now()) || !room.Online || room.Status() == status {
		return room.snapshot(), nil
	}
	switch status {
	case RoomOffline:
		room.end()
	case RoomAnchorAway:
		room.setAnchorAwayUntil(time.Now().UTC().Add(s.anchorAwayTimeout))
	case RoomOnline, RoomLiveShow:
		room.LiveShow = status == RoomLiveShow
		if room.AnchorAway {
			room.setAnchorAwayUntil(time.Time{})
		} else {
			room.publish(Event{Type: "room_status", UserID: room.OwnerID, RoomStatus: status, CreatedAt: time.Now().UTC()})
		}
	}
	return room.snapshot(), nil
}
