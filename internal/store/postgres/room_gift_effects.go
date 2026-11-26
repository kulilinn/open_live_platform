package postgres

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/gift"
	"open_live_platform/internal/live"
)

func (s *Store) ReduceGiftDuration(ctx context.Context, roomID, actor int64, ratio float64) error {
	return s.change(ctx, roomID, func(r *roomChange) error {
		info, err := scanRoom(r.tx.QueryRow(ctx, roomQuery+" WHERE id=$1", roomID))
		if err != nil {
			return err
		}
		if info.OwnerID != actor {
			return live.ErrForbidden
		}
		_, err = r.tx.Exec(ctx, `INSERT INTO room_gift_effect_settings(room_id,reduce_time_ratio) VALUES($1,$2)
			ON CONFLICT(room_id) DO UPDATE SET reduce_time_ratio=EXCLUDED.reduce_time_ratio`, roomID, ratio)
		return err
	})
}

const giftDurationRatio = `SELECT COALESCE((SELECT reduce_time_ratio FROM room_gift_effect_settings WHERE room_id=$1),0)`

func (s *Store) GiftDurationReduction(ctx context.Context, roomID int64) (float64, error) {
	var ratio float64
	err := s.pool.QueryRow(ctx, giftDurationRatio, roomID).Scan(&ratio)
	return ratio, err
}

func (r *roomChange) reserveGiftEffect(order gift.Order) error {
	if order.Item.EffectControlType == gift.RefuelingEffect {
		return r.reserveRocketRefuel(order)
	}
	if !order.Item.QueuesEffect() {
		return nil
	}
	if order.Item.EffectControlType == gift.RocketEffect {
		var busy bool
		err := r.tx.QueryRow(r.ctx, `SELECT EXISTS(SELECT 1 FROM room_gift_effects e JOIN gift_orders o ON o.id=e.order_id
			WHERE e.room_id=$1 AND e.live_id=$2 AND e.effect=6 AND e.ended_at IS NULL
			AND (o.state=0 OR (o.state=1 AND e.end_at>statement_timestamp())))`, r.id, order.Input.LiveID).Scan(&busy)
		if err != nil {
			return err
		}
		if busy {
			return gift.ErrEffectBusy
		}
		if err := r.reserveRocketLaunch(order); err != nil {
			return err
		}
	}
	_, err := r.tx.Exec(r.ctx, `INSERT INTO room_gift_effects(order_id,room_id,live_id,effect) VALUES($1,$2,$3,$4)`,
		order.ID, r.id, order.Input.LiveID, order.Item.EffectControlType)
	return err
}

func (r *roomChange) startGiftEffect(order gift.Order) error {
	if !order.Item.QueuesEffect() {
		return nil
	}
	start := order.PaidAt
	duration := order.Item.EffectSeconds()
	if order.Item.EffectControlType != gift.RocketEffect {
		var ratio float64
		if err := r.tx.QueryRow(r.ctx, giftDurationRatio, r.id).Scan(&ratio); err != nil {
			return err
		}
		duration = max(1, int64(float64(duration)*(100-ratio)/100))
		err := r.tx.QueryRow(r.ctx, `SELECT GREATEST($3,max(end_at)) FROM room_gift_effects
			WHERE room_id=$1 AND live_id=$2 AND effect BETWEEN 2 AND 5 AND ended_at IS NULL`, r.id, order.Input.LiveID, start).Scan(&start)
		if err != nil {
			return err
		}
	}
	end := start.Add(time.Duration(duration) * time.Second)
	var speed int32
	if order.Item.EffectControlType == gift.RocketEffect {
		speed = 10
		if order.Item.ParameterTwo != nil {
			speed = max(speed, *order.Item.ParameterTwo)
		}
		speed = min(speed, 100)
	}
	_, err := r.tx.Exec(r.ctx, `UPDATE room_gift_effects SET start_at=$2,end_at=$3,speed=$4,motion_at=$2,
		attenuation=COALESCE((SELECT attenuation FROM rocket_launch_amounts WHERE order_id=$1),0) WHERE order_id=$1`,
		order.ID, start, end, speed)
	return err
}

func scanRoomEffect(row pgx.CollectableRow) (gift.RoomEffect, error) {
	var result gift.RoomEffect
	var ended *time.Time
	var motion *time.Time
	var paidAt *time.Time
	err := row.Scan(&result.Start, &result.End, &ended, &result.Speed, &result.FinalResult, &motion, &result.Distance, &result.Attenuation,
		&result.Order.ID, &result.Order.Input, &result.Order.Key,
		&result.Order.PaymentOrder, &result.Order.Item, &result.Order.Sender, &result.Order.Receiver, &result.Order.SenderRole,
		&result.Order.Cost, &result.Order.Income, &result.Order.State, &paidAt, &result.Order.Receipt, &result.Order.Result)
	if ended != nil {
		result.Ended = *ended
	}
	if motion != nil {
		result.MotionAt = *motion
	}
	if paidAt != nil {
		result.Order.PaidAt = paidAt.UTC()
	}
	return result, err
}

const selectRoomEffects = `SELECT e.start_at,e.end_at,e.ended_at,e.speed,e.final_result,e.motion_at,e.distance,e.attenuation,
	o.id,o.request,o.request_key,o.payment_order,o.item,
	o.sender_info,o.receiver_info,o.sender_role,o.cost,o.income,o.state,o.paid_at,o.receipt,o.result
	FROM room_gift_effects e JOIN gift_orders o ON o.id=e.order_id WHERE e.room_id=$1 AND e.live_id=$2 AND e.start_at IS NOT NULL`

const dueHardware = `e.effect BETWEEN 2 AND 5 AND e.announced_at IS NULL AND e.ended_at IS NULL
	AND e.start_at<=statement_timestamp() AND e.end_at>statement_timestamp() AND r.online AND e.live_id=r.live_id`

func (s *Store) SweepRoomGiftEffects(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT e.room_id FROM room_gift_effects e JOIN rooms r ON r.id=e.room_id
		WHERE `+dueHardware+` ORDER BY e.room_id LIMIT 64`)
	if err != nil {
		return err
	}
	rooms, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	for _, roomID := range rooms {
		err := s.change(ctx, roomID, func(r *roomChange) error {
			var id, actor, liveID int64
			err := r.tx.QueryRow(ctx, `SELECT e.order_id,g.sender_id,e.live_id FROM room_gift_effects e
				JOIN rooms r ON r.id=e.room_id JOIN gift_orders g ON g.id=e.order_id WHERE e.room_id=$1 AND `+dueHardware+
				` ORDER BY e.start_at LIMIT 1`, roomID).Scan(&id, &actor, &liveID)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			if _, err := r.tx.Exec(ctx, `UPDATE room_gift_effects SET announced_at=statement_timestamp() WHERE order_id=$1`, id); err != nil {
				return err
			}
			r.event = live.Event{Type: "gift_effect", MessageID: id, UserID: actor, LiveID: liveID}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return s.finishExpiredRockets(ctx)
}

func (s *Store) RoomHardware(ctx context.Context, roomID, liveID int64) ([]gift.RoomEffect, error) {
	rows, err := s.pool.Query(ctx, selectRoomEffects+` AND e.effect BETWEEN 2 AND 5 AND e.ended_at IS NULL
		AND e.end_at>statement_timestamp() ORDER BY e.start_at,e.order_id LIMIT 31`, roomID, liveID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanRoomEffect)
}

func (s *Store) RoomRocket(ctx context.Context, roomID, liveID int64) (*gift.RoomEffect, error) {
	rows, err := s.pool.Query(ctx, selectRoomEffects+` AND e.effect=6 ORDER BY e.start_at DESC,e.order_id DESC LIMIT 1`, roomID, liveID)
	if err != nil {
		return nil, err
	}
	value, err := pgx.CollectOneRow(rows, scanRoomEffect)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &value, err
}

func (s *Store) EndRoomEffect(ctx context.Context, roomID, actor int64, rocket bool, giftID int64) error {
	return s.change(ctx, roomID, func(r *roomChange) error {
		info, err := scanRoom(r.tx.QueryRow(ctx, roomQuery+" WHERE id=$1", roomID))
		if err != nil {
			return err
		}
		if info.OwnerID != actor {
			return live.ErrForbidden
		}
		_, err = r.tx.Exec(ctx, `UPDATE room_gift_effects SET ended_at=statement_timestamp() WHERE order_id=(
			SELECT e.order_id FROM room_gift_effects e JOIN gift_orders g ON e.order_id=g.id
			WHERE e.room_id=$1 AND e.live_id=$2 AND e.ended_at IS NULL AND e.start_at<=statement_timestamp()
			AND e.end_at>=statement_timestamp()-interval '2 seconds'
			AND (($3::boolean AND e.effect=6) OR (NOT $3 AND e.effect BETWEEN 2 AND 5 AND (g.item->>'id')::bigint=$4))
			AND e.end_at<=statement_timestamp()
			ORDER BY e.start_at,e.order_id LIMIT 1)`, roomID, info.Session.LiveID, rocket, giftID)
		return err
	})
}

func (s *Store) RocketRank(ctx context.Context, orderID int64) ([]gift.RocketContribution, error) {
	return readRocketRank(ctx, s.pool, orderID)
}

type rocketRankQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func readRocketRank(ctx context.Context, db rocketRankQueryer, orderID int64) ([]gift.RocketContribution, error) {
	rows, err := db.Query(ctx, `SELECT c.user_id,u.account,COALESCE(u.profile->>'Photo',''),c.score
		FROM room_rocket_contributions c JOIN users u ON u.id=c.user_id WHERE c.order_id=$1
		ORDER BY c.score DESC,c.user_id LIMIT 5`, orderID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (gift.RocketContribution, error) {
		var value gift.RocketContribution
		err := row.Scan(&value.User, &value.Name, &value.Photo, &value.Score)
		return value, err
	})
}

func (s *Store) RocketWasLaunched(ctx context.Context, roomID, userID int64) (bool, error) {
	var found bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM room_gift_effects e JOIN gift_orders o ON e.order_id=o.id
		WHERE e.room_id=$1 AND e.effect=6 AND o.state=1 AND o.sender_id=$2)`, roomID, userID).Scan(&found)
	return found, err
}

func (s *Store) IncreaseRocketSpeed(ctx context.Context, roomID, anchor, user int64, likes int32, coefficient float64) error {
	return s.change(ctx, roomID, func(r *roomChange) error {
		info, err := scanRoom(r.tx.QueryRow(ctx, roomQuery+" WHERE id=$1", roomID))
		if err != nil {
			return err
		}
		if anchor != info.OwnerID {
			return live.ErrForbidden
		}
		if !info.Online {
			return live.ErrRoomClosed
		}
		if _, err := r.memberLiveID(user); err != nil {
			return err
		}
		var id int64
		var flight gift.RoomEffect
		err = r.tx.QueryRow(ctx, `SELECT order_id,speed,start_at,end_at,motion_at,distance,attenuation
			FROM room_gift_effects WHERE room_id=$1 AND live_id=$2 AND effect=6
			AND ended_at IS NULL AND end_at>statement_timestamp() ORDER BY start_at DESC LIMIT 1`, roomID, info.Session.LiveID).
			Scan(&id, &flight.Speed, &flight.Start, &flight.End, &flight.MotionAt, &flight.Distance, &flight.Attenuation)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		speed, distance, motionAt := flight.Motion(time.Now())
		if coefficient <= 0 || speed <= 0 {
			return nil
		}
		increase := int32(math.Min(float64(100-speed), float64(likes)/(float64(speed)*coefficient)))
		if increase <= 0 {
			return nil
		}
		_, err = r.tx.Exec(ctx, `UPDATE room_gift_effects SET speed=$2,distance=$3,motion_at=$4 WHERE order_id=$1`,
			id, speed+increase, distance, motionAt)
		return err
	})
}
