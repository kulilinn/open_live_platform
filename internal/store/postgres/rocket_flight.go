package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/live"
	"open_live_platform/internal/subscription"
)

const rocketDue = `e.effect=6 AND e.final_result IS NULL AND e.start_at IS NOT NULL
	AND (e.end_at<=statement_timestamp() OR e.ended_at IS NOT NULL OR EXISTS(
		SELECT 1 FROM room_broadcast_metrics b WHERE b.live_id=e.live_id AND b.ended_at IS NOT NULL))`

func (s *Store) finishExpiredRockets(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT e.room_id FROM room_gift_effects e WHERE `+rocketDue+` ORDER BY e.room_id LIMIT 64`)
	if err != nil {
		return err
	}
	rooms, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	for _, roomID := range rooms {
		if err := s.change(ctx, roomID, func(r *roomChange) error { return r.finishRocket() }); err != nil {
			return err
		}
	}
	return nil
}

func (r *roomChange) finishRocket() error {
	var liveID int64
	err := r.tx.QueryRow(r.ctx, `SELECT e.live_id FROM room_gift_effects e WHERE e.room_id=$1 AND `+rocketDue+
		` ORDER BY e.end_at,e.order_id LIMIT 1`, r.id).Scan(&liveID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	rows, err := r.tx.Query(r.ctx, selectRoomEffects+` AND `+rocketDue+` ORDER BY e.end_at,e.order_id LIMIT 1`, r.id, liveID)
	if err != nil {
		return err
	}
	effect, err := pgx.CollectOneRow(rows, scanRoomEffect)
	if err != nil {
		return err
	}

	var end time.Time
	var activeRoom bool
	if err := r.tx.QueryRow(r.ctx, `SELECT LEAST($2::timestamptz,$3::timestamptz,b.ended_at),r.online AND r.live_id=$1
		FROM room_broadcast_metrics b JOIN rooms r ON r.id=b.room_id WHERE b.live_id=$1`, liveID, effect.End, nullableTime(effect.Ended)).
		Scan(&end, &activeRoom); err != nil {
		return err
	}
	end = maxTime(effect.Start, end)
	_, distance, _ := effect.Motion(end)
	participants, rank, err := readRocketFlightSummary(r.ctx, r.tx, effect.Order.ID, effect.Order.Input.ReceiverID)
	if err != nil {
		return err
	}
	payload := map[string]any{
		"anchor_id": strconv.FormatInt(effect.Order.Input.ReceiverID, 10), "anchor_name": effect.Order.Receiver.Name,
		"anchor_photo": effect.Order.Receiver.Photo, "sender_name": effect.Order.Sender.Name, "sender_avatar": effect.Order.Sender.Photo,
		"flight_distance": distance, "flight_duration": int64(end.Sub(effect.Start) / time.Second),
		"participants": participants, "contribute_user": rank,
	}
	data, err := json.Marshal(map[string]any{
		"id": strconv.FormatInt(rocketMessageID(effect.Order.ID, 7), 10), "channel": "realtime", "room": effect.Order.Input.Room, "priority": 0,
		"sender": strconv.FormatInt(effect.Order.Input.SenderID, 10), "timestamp": strconv.FormatInt(end.Unix(), 10),
		"message": map[string]any{"msgGroup": "gift", "msgType": 7, "message": payload},
	})
	if err != nil {
		return err
	}
	if _, err = r.tx.Exec(r.ctx, `UPDATE room_gift_effects SET final_result=$2,ended_at=$3 WHERE order_id=$1`, effect.Order.ID, data, end); err != nil {
		return err
	}
	if activeRoom {
		r.event = live.Event{Type: "rocket_final", UserID: effect.Order.Input.SenderID, CreatedAt: end, MessageID: effect.Order.ID, LiveID: liveID}
	}
	return nil
}

func nullableTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func readRocketFlightSummary(ctx context.Context, tx pgx.Tx, orderID, anchorID int64) (int64, []map[string]any, error) {
	var count int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM room_rocket_contributions WHERE order_id=$1`, orderID).Scan(&count); err != nil {
		return 0, nil, err
	}
	rows, err := tx.Query(ctx, `SELECT c.user_id,u.account,COALESCE(u.profile->>'Photo',''),c.score,c.combo,c.multiplier
		FROM room_rocket_contributions c JOIN users u ON u.id=c.user_id WHERE c.order_id=$1
		ORDER BY c.score DESC,c.user_id LIMIT 5`, orderID)
	if err != nil {
		return 0, nil, err
	}
	type contribution struct {
		actor                  int64
		name, photo            string
		score, combo, critical int64
	}
	ranked, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (contribution, error) {
		var c contribution
		err := row.Scan(&c.actor, &c.name, &c.photo, &c.score, &c.combo, &c.critical)
		return c, err
	})
	if err != nil {
		return 0, nil, err
	}
	rank := make([]map[string]any, 0, len(ranked))
	for _, c := range ranked {
		platform, fan, err := subscriptionExperience(ctx, tx, c.actor, anchorID)
		if err != nil {
			return 0, nil, err
		}
		platformLevel, fanLevel := subscription.ExperienceLevels(platform, fan)
		rank = append(rank, map[string]any{"id": strconv.FormatInt(c.actor, 10), "name": c.name, "photo": c.photo, "contribute": c.score,
			"max_combo": c.combo, "max_critical_strike": c.critical, "platform_level": platformLevel, "fan_level": fanLevel})
	}
	return count, rank, nil
}
