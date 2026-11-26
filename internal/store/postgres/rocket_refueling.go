package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/gift"
)

func (s *Store) ConfigureRocketRewards(ctx context.Context, rules *gift.RocketRules) error {
	if rules == nil {
		return nil
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO rocket_jackpot(balance) VALUES($1) ON CONFLICT DO NOTHING`, rules.InitialJackpot); err != nil {
		return err
	}
	s.rocketRules = rules
	return nil
}

func (r *roomChange) reserveRocketRefuel(order gift.Order) error {
	rules := r.rocketRules
	if rules == nil {
		return gift.ErrRocketUnavailable
	}
	var launchID, flightSeconds int64
	err := r.tx.QueryRow(r.ctx, `SELECT order_id,floor(extract(epoch FROM end_at-start_at))::bigint
		FROM room_gift_effects WHERE room_id=$1 AND live_id=$2 AND effect=6 AND ended_at IS NULL
		AND end_at>statement_timestamp()+$3::interval ORDER BY start_at DESC LIMIT 1`,
		r.id, order.Input.LiveID, time.Duration(rules.MinSeconds)*time.Second).Scan(&launchID, &flightSeconds)
	if errors.Is(err, pgx.ErrNoRows) {
		return gift.ErrCannotRefuel
	}
	if err != nil {
		return err
	}
	var available int64
	err = r.tx.QueryRow(r.ctx, `SELECT balance-reserved FROM rocket_jackpot WHERE singleton FOR UPDATE`).Scan(&available)
	if errors.Is(err, pgx.ErrNoRows) {
		return gift.ErrRocketUnavailable
	}
	if err != nil {
		return err
	}
	plan, err := rules.Draw(order, available, flightSeconds)
	if err != nil {
		return err
	}
	if _, err := r.tx.Exec(r.ctx, `UPDATE rocket_jackpot SET reserved=reserved+$1 WHERE singleton`, plan.Reserved); err != nil {
		return err
	}
	_, err = r.tx.Exec(r.ctx, `INSERT INTO rocket_refuel_orders(order_id,launch_id,reserved,plan) VALUES($1,$2,$3,$4)`,
		order.ID, launchID, plan.Reserved, plan)
	return err
}

func (r *roomChange) reserveRocketLaunch(order gift.Order) error {
	if r.rocketRules == nil {
		return nil
	}
	if order.Cost > math.MaxInt32 {
		return gift.ErrUnsupported
	}
	add, fee := r.rocketRules.LaunchAmounts(order.Cost)
	_, err := r.tx.Exec(r.ctx, `INSERT INTO rocket_launch_amounts(order_id,contribution,platform_points,attenuation) VALUES($1,$2,$3,$4)`,
		order.ID, add, fee, r.rocketRules.SpeedAttenuation)
	return err
}

func (r *roomChange) settleRocketLaunch(order gift.Order) error {
	var plan gift.RefuelPlan
	err := r.tx.QueryRow(r.ctx, `SELECT contribution,platform_points FROM rocket_launch_amounts WHERE order_id=$1`, order.ID).
		Scan(&plan.Contribution, &plan.PlatformIncome)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return r.moveRocketPool(order.ID, plan)
}

func (r *roomChange) moveRocketPool(orderID int64, plan gift.RefuelPlan) error {
	var balance int64
	err := r.tx.QueryRow(r.ctx, `UPDATE rocket_jackpot SET balance=balance+$1-$2,reserved=reserved-$3 WHERE singleton RETURNING balance`,
		plan.Contribution, plan.Drawn, plan.Reserved).Scan(&balance)
	if err != nil {
		return err
	}
	_, err = r.tx.Exec(r.ctx, `INSERT INTO rocket_pool_ledger(order_id,contribution,platform_points,drawn,balance) VALUES($1,$2,$3,$4,$5)`,
		orderID, plan.Contribution, plan.PlatformIncome, plan.Drawn, balance)
	return err
}

func (r *roomChange) releaseRocketRefuel(orderID int64) error {
	_, err := r.tx.Exec(r.ctx, `UPDATE rocket_jackpot SET reserved=rocket_jackpot.reserved-p.reserved
		FROM rocket_refuel_orders p WHERE singleton AND p.order_id=$1`, orderID)
	return err
}

func (r *roomChange) settleRocketRefuel(order gift.Order) (int64, error) {
	var plan gift.RefuelPlan
	var launchID int64
	if err := r.tx.QueryRow(r.ctx, `SELECT launch_id,plan FROM rocket_refuel_orders WHERE order_id=$1`, order.ID).Scan(&launchID, &plan); err != nil {
		return 0, err
	}
	if err := r.moveRocketPool(order.ID, plan); err != nil {
		return 0, err
	}
	for i, reward := range plan.Rewards {
		_, err := r.tx.Exec(r.ctx, `INSERT INTO rocket_refuel_awards(order_id,draw,anchor_id,gift_id,gift_name,gift_icon,income,settled_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, order.ID, i, order.Input.ReceiverID, reward.Gift.ID, reward.Gift.Name,
			reward.Gift.Icon, int64(reward.Gift.IncomeStream), order.PaidAt)
		if err != nil {
			return 0, err
		}
	}
	_, err := r.tx.Exec(r.ctx, `INSERT INTO room_rocket_contributions(order_id,user_id,score,combo,multiplier,platform_exp)
		VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(order_id,user_id) DO UPDATE SET
			score=room_rocket_contributions.score+EXCLUDED.score,combo=room_rocket_contributions.combo+EXCLUDED.combo,
			multiplier=GREATEST(room_rocket_contributions.multiplier,EXCLUDED.multiplier),
			platform_exp=room_rocket_contributions.platform_exp+EXCLUDED.platform_exp`,
		launchID, order.Input.SenderID, plan.Score, int64(order.Input.Quantity), plan.Multiplier, plan.PlatformExp)
	if err != nil {
		return 0, err
	}
	if plan.FuelMillis == 0 {
		return launchID, nil
	}
	var ratio float64
	if err := r.tx.QueryRow(r.ctx, giftDurationRatio, r.id).Scan(&ratio); err != nil {
		return 0, err
	}
	millis := int64(float64(plan.FuelMillis) * (100 - ratio) / 100)
	var remainder int64
	var end time.Time
	var ended *time.Time
	err = r.tx.QueryRow(r.ctx, `SELECT fuel_remainder,end_at,ended_at FROM room_gift_effects WHERE order_id=$1`, launchID).Scan(&remainder, &end, &ended)
	if err != nil {
		return 0, err
	}
	if ended != nil || !order.PaidAt.Before(end) {
		return launchID, nil
	}
	if millis < 1000 {
		millis += remainder
		if millis >= 1000 {
			remainder = 0
		} else {
			remainder, millis = millis, 0
		}
	}
	extend := time.Duration(millis/1000) * time.Second
	_, err = r.tx.Exec(r.ctx, `UPDATE room_gift_effects SET end_at=$2,fuel_remainder=$3 WHERE order_id=$1`, launchID, end.Add(extend), remainder)
	if err == nil && extend > 0 {
		_, err = r.tx.Exec(r.ctx, `UPDATE rocket_refuel_orders SET added_millis=$2,end_at_after=$3 WHERE order_id=$1`,
			order.ID, extend.Milliseconds(), end.Add(extend))
	}
	return launchID, err
}

func rocketRefuelFrames(ctx context.Context, conn *pgx.Conn, signal roomSignal) ([][]byte, error) {
	order, err := scanGiftOrder(conn.QueryRow(ctx, `SELECT `+giftOrderColumns+` FROM gift_orders
		WHERE id=$1 AND room_id=$2 AND state=1 AND EXISTS(SELECT 1 FROM rooms r WHERE r.id=$2 AND r.online AND r.live_id=gift_orders.live_id)`,
		signal.MessageID, signal.RoomID))
	if errors.Is(err, gift.ErrOrderNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var launchID, millis int64
	var end *time.Time
	if err := conn.QueryRow(ctx, `SELECT launch_id,added_millis,end_at_after FROM rocket_refuel_orders WHERE order_id=$1`, order.ID).
		Scan(&launchID, &millis, &end); err != nil {
		return nil, err
	}
	rank, err := readRocketRank(ctx, conn, launchID)
	if err != nil {
		return nil, err
	}
	payloads := []struct {
		kind    int
		channel string
		data    any
	}{
		{6, "realtime", map[string]any{"user_name": order.Sender.Name, "combo": order.Input.Quantity, "context": ""}},
		{9, "rocket_rank", rank},
	}
	if end != nil && millis > 0 {
		payloads = append(payloads, struct {
			kind    int
			channel string
			data    any
		}{16, "realtime", map[string]any{
			"refuel_time": millis, "user_name": order.Sender.Name, "user_avatar": order.Sender.Photo, "rocket_end_time": end.Unix(),
		}})
	}
	frames := make([][]byte, 0, len(payloads))
	for _, part := range payloads {
		frame, err := json.Marshal(map[string]any{
			"id": strconv.FormatInt(rocketMessageID(order.ID, part.kind), 10), "room": order.Input.Room, "channel": part.channel, "priority": 0,
			"sender": strconv.FormatInt(order.Input.SenderID, 10), "timestamp": strconv.FormatInt(order.PaidAt.Unix(), 10),
			"message": map[string]any{"msgGroup": "gift", "msgType": part.kind, "message": part.data},
		})
		if err != nil {
			return nil, err
		}
		frames = append(frames, frame)
	}
	return frames, nil
}

func rocketMessageID(orderID int64, kind int) int64 {
	key := sha256.Sum256([]byte("rocket-" + strconv.FormatInt(orderID, 10) + "-" + strconv.Itoa(kind)))
	return int64(binary.BigEndian.Uint64(key[:8]) & math.MaxInt64)
}
