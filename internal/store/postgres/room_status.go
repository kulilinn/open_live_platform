package postgres

import (
	"context"
	"time"

	"open_live_platform/internal/live"
)

func (s *Store) SetRoomStatus(ctx context.Context, id, actor int64, status live.RoomStatus) (live.Room, error) {
	var room live.Room
	err := s.change(ctx, id, func(r *roomChange) error {
		var err error
		room, err = scanRoom(r.tx.QueryRow(ctx, roomQuery+" WHERE id=$1", id))
		if err != nil {
			return err
		}
		if room.OwnerID != actor || status > live.RoomAnchorAway {
			return live.ErrForbidden
		}
		if room.Limited(time.Now()) || !room.Online || room.Status() == status {
			return nil
		}
		switch status {
		case live.RoomOffline:
			err = r.close(actor)
		case live.RoomAnchorAway:
			err = s.setAnchorAway(r, actor, true)
		case live.RoomOnline, live.RoomLiveShow:
			var at time.Time
			err = r.tx.QueryRow(ctx, `UPDATE rooms SET live_show=$2,anchor_away=false,anchor_away_until=NULL,updated_at=statement_timestamp()
				WHERE id=$1 RETURNING updated_at`, id, status == live.RoomLiveShow).Scan(&at)
			r.event = live.Event{Type: "room_status", UserID: actor, CreatedAt: at, RoomStatus: status}
		}
		if err != nil {
			return err
		}
		room, err = scanRoom(r.tx.QueryRow(ctx, roomQuery+" WHERE id=$1", id))
		return err
	})
	return room, err
}
