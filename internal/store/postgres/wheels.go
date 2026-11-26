package postgres

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/gift"
	"open_live_platform/internal/live"
	"open_live_platform/internal/payment"
)

func (s *Store) WheelConfig(ctx context.Context, anchorID int64) (gift.WheelConfig, error) {
	return readWheelConfig(s.pool.QueryRow(ctx, `SELECT price,options,open_status FROM room_wheel_configs WHERE anchor_id=$1`, anchorID), anchorID)
}

func readWheelConfig(row pgx.Row, anchorID int64) (gift.WheelConfig, error) {
	result := gift.WheelConfig{AnchorID: anchorID, OpenStatus: 1, Options: []string{}}
	err := row.Scan(&result.Price, &result.Options, &result.OpenStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, nil
	}
	return result, err
}

func (s *Store) UpdateWheelConfig(ctx context.Context, anchorID int64, change gift.WheelChange) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO room_wheel_configs(anchor_id,price,options,open_status)
		VALUES ($1,COALESCE($2::bigint,0),COALESCE($3::text[],'{}'),COALESCE($4::smallint,1))
		ON CONFLICT(anchor_id) DO UPDATE SET price=COALESCE($2::bigint,room_wheel_configs.price),
		options=COALESCE($3::text[],room_wheel_configs.options),open_status=COALESCE($4::smallint,room_wheel_configs.open_status)`,
		anchorID, change.Price, change.Options, change.Status)
	return err
}

func (s *Store) ReserveWheel(ctx context.Context, candidate gift.WheelOrder) (gift.WheelOrder, bool, error) {
	var result gift.WheelOrder
	var created bool
	err := s.change(ctx, candidate.RoomID, func(r *roomChange) error {
		err := r.tx.QueryRow(ctx, `SELECT order_data FROM room_wheel_orders WHERE user_id=$1 AND request_key=$2`,
			candidate.UserID, candidate.Key).Scan(&result)
		if err == nil {
			if result.WheelInput != candidate.WheelInput {
				return gift.ErrOrderConflict
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		current, err := r.memberLiveID(candidate.UserID)
		if err != nil {
			return err
		}
		if current != candidate.LiveID || r.id != candidate.AnchorID || candidate.UserID == candidate.AnchorID {
			return live.ErrForbidden
		}
		config, err := readWheelConfig(r.tx.QueryRow(ctx, `SELECT price,options,open_status FROM room_wheel_configs WHERE anchor_id=$1 FOR SHARE`,
			candidate.AnchorID), candidate.AnchorID)
		if err != nil {
			return err
		}
		if config.OpenStatus != 0 || config.Price < 10 || len(config.Options) < 1 {
			return gift.ErrWheelClosed
		}
		candidate.Price, candidate.Result.Value = config.Price, config.Options[rand.IntN(len(config.Options))]
		tag, err := r.tx.Exec(ctx, `INSERT INTO room_wheel_orders(id,room_id,live_id,anchor_id,user_id,request_key,state,order_data)
			VALUES($1,$2,$3,$4,$5,$6,0,$7) ON CONFLICT(user_id,request_key) DO NOTHING`, candidate.ID, candidate.RoomID,
			candidate.LiveID, candidate.AnchorID, candidate.UserID, candidate.Key, candidate)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			if err := r.tx.QueryRow(ctx, `SELECT order_data FROM room_wheel_orders WHERE user_id=$1 AND request_key=$2`,
				candidate.UserID, candidate.Key).Scan(&result); err != nil {
				return err
			}
			if result.WheelInput != candidate.WheelInput {
				return gift.ErrOrderConflict
			}
			return nil
		}
		result, created = candidate, true
		return nil
	})
	return result, created, err
}

func (s *Store) FinishWheel(ctx context.Context, order gift.WheelOrder, receipt payment.Receipt, declined bool) (gift.WheelOrder, error) {
	var result gift.WheelOrder
	err := s.change(ctx, order.RoomID, func(r *roomChange) error {
		err := r.tx.QueryRow(ctx, `SELECT order_data FROM room_wheel_orders WHERE id=$1 AND room_id=$2`, order.ID, order.RoomID).Scan(&result)
		if err != nil || result.State != gift.Pending {
			return err
		}
		var paidAt *time.Time
		var providerID *int64
		if declined {
			result.State = gift.Declined
		} else {
			if receipt.OrderID <= 0 || receipt.TotalPoints != result.Price {
				return gift.ErrPending
			}
			result.State, result.Receipt, result.Result.Time = gift.Paid, receipt, time.Now().UTC()
			paidAt, providerID = &result.Result.Time, &receipt.OrderID
		}
		_, err = r.tx.Exec(ctx, `UPDATE room_wheel_orders SET state=$2,order_data=$3,provider_id=$4,paid_at=$5 WHERE id=$1`,
			result.ID, result.State, result, providerID, paidAt)
		if err != nil || declined {
			return err
		}
		var active bool
		if err := r.tx.QueryRow(ctx, `SELECT online AND live_id=$2 FROM rooms WHERE id=$1`, result.RoomID, result.LiveID).Scan(&active); err != nil {
			return err
		}
		if active {
			r.event = live.Event{Type: "wheel", UserID: result.UserID, MessageID: result.ID}
		}
		return nil
	})
	return result, err
}

func (s *Store) WheelHistory(ctx context.Context, roomID, liveID, page, size int64) ([]gift.WheelResult, int64, error) {
	var count int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM room_wheel_orders WHERE room_id=$1 AND live_id=$2 AND state=1`, roomID, liveID).Scan(&count); err != nil {
		return nil, 0, err
	}
	rows, err := s.pool.Query(ctx, `SELECT order_data->'Result' FROM room_wheel_orders WHERE room_id=$1 AND live_id=$2 AND state=1
		ORDER BY paid_at,id LIMIT $3 OFFSET $4`, roomID, liveID, size, (page-1)*size)
	if err != nil {
		return nil, 0, err
	}
	result, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (gift.WheelResult, error) {
		var item gift.WheelResult
		err := row.Scan(&item)
		return item, err
	})
	return result, (count + size - 1) / size, err
}

func (s *Store) WheelRecommendations(ctx context.Context) (map[string][]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT title,suggestion FROM room_wheel_recommendations ORDER BY title,suggestion`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string][]string)
	for rows.Next() {
		var title, suggestion string
		if err := rows.Scan(&title, &suggestion); err != nil {
			return nil, err
		}
		result[title] = append(result[title], suggestion)
	}
	return result, rows.Err()
}
