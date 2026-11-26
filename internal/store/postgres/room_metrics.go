package postgres

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/live"
)

func (r *roomChange) updateMaxRoomAudience() error {
	_, err := r.tx.Exec(r.ctx, `UPDATE room_broadcast_metrics b SET max_audience=GREATEST(b.max_audience,
			(SELECT count(*) FROM room_members WHERE room_id=$1 AND user_id<>r.owner_id))
		FROM rooms r WHERE b.live_id=r.live_id AND r.id=$1`, r.id)
	return err
}

func (s *Store) AddRoomLikes(ctx context.Context, roomID, liveID, userID, count int64) (int64, error) {
	if count <= 0 {
		return 0, live.ErrLikeCount
	}
	var total int64
	err := s.change(live.WithRoomCommandScope(ctx, roomID, liveID), roomID, func(r *roomChange) error {
		var err error
		total, err = r.addRoomLikes(liveID, userID, count)
		return err
	})
	return total, err
}

func (r *roomChange) addRoomLikes(liveID, userID, count int64) (int64, error) {
	var total int64
	err := r.tx.QueryRow(r.ctx, `UPDATE room_broadcast_metrics SET like_total=like_total+$2
		WHERE live_id=$1 AND like_total<=$3 RETURNING like_total`, liveID, count, int64(math.MaxInt64)-count).Scan(&total)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, live.ErrLikeCount
	}
	if err != nil {
		return 0, err
	}
	if _, err := r.tx.Exec(r.ctx, `INSERT INTO room_broadcast_viewer_likes(live_id,user_id,likes) VALUES($1,$2,$3)
		ON CONFLICT(live_id,user_id) DO UPDATE SET likes=room_broadcast_viewer_likes.likes+EXCLUDED.likes`, liveID, userID, count); err != nil {
		return 0, err
	}
	r.event = live.RoomLikeEvent(r.id, liveID, userID, total, time.Now().UTC())
	return total, nil
}

func (s *Store) RoomSettlement(ctx context.Context, liveID, ownerID int64) (live.RoomSettlement, error) {
	var data live.RoomSettlement
	var owner int64
	err := s.pool.QueryRow(ctx, `SELECT r.owner_id,
		(EXTRACT(EPOCH FROM date_trunc('second',COALESCE(b.ended_at,statement_timestamp())))-
		 EXTRACT(EPOCH FROM date_trunc('second',b.started_at)))::bigint,
		COALESCE(b.legacy_income,g.income,0),b.max_audience, COALESCE(b.legacy_new_fans,f.new_fans,0),COALESCE(b.legacy_gift_fans,g.senders,0),r.banner
		FROM room_broadcast_metrics b JOIN rooms r ON b.room_id=r.id
		LEFT JOIN LATERAL (SELECT SUM(income)::bigint AS income,count(DISTINCT sender_id) AS senders
			FROM room_gift_income WHERE live_id=b.live_id AND room_id=b.room_id) g ON r.owner_id=$2
		LEFT JOIN LATERAL (SELECT count(*) AS new_fans FROM
			(SELECT min(created_at) AS first FROM subscription_members WHERE anchor_id=r.owner_id GROUP BY fan_id) m
			WHERE m.first>=b.started_at AND m.first<=COALESCE(b.ended_at,statement_timestamp())) f ON r.owner_id=$2
		WHERE b.live_id=$1`, liveID, ownerID).Scan(&owner, &data.Time, &data.Income, &data.FanCount, &data.NewFanCount, &data.GiftUserCount, &data.Banner)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return live.RoomSettlement{}, live.ErrRoomNotFound
	case err != nil:
		return live.RoomSettlement{}, err
	case owner != ownerID:
		return live.RoomSettlement{}, live.ErrForbidden
	default:
		return data, nil
	}
}

func (s *Store) ReportRoom(ctx context.Context, report live.RoomReport) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO room_reports(room,reporter_id,subject,description) VALUES($1,$2,$3,$4)`,
		report.Room, report.ReporterID, report.Subject, report.Description)
	return err
}
