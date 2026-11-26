package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/gift"
	"open_live_platform/internal/live"
	"open_live_platform/internal/payment"
)

var _ gift.OrderStore = (*Store)(nil)

const giftOrderColumns = `id,request,request_key,payment_order,item,sender_info,receiver_info,
    sender_role,cost,income,state,paid_at,receipt,result`
const selectGiftOrder = "SELECT " + giftOrderColumns + " FROM gift_orders WHERE sender_id=$1 AND request_key=$2"

func scanGiftOrder(row pgx.Row) (gift.Order, error) {
	var result gift.Order
	var paidAt *time.Time
	err := row.Scan(&result.ID, &result.Input, &result.Key, &result.PaymentOrder, &result.Item, &result.Sender, &result.Receiver,
		&result.SenderRole, &result.Cost, &result.Income, &result.State, &paidAt, &result.Receipt, &result.Result)
	if paidAt != nil {
		result.PaidAt = paidAt.UTC()
	}
	if errors.Is(err, pgx.ErrNoRows) {
		err = gift.ErrOrderNotFound
	}
	return result, err
}

func (s *Store) FindGiftOrder(ctx context.Context, senderID int64, key string) (gift.Order, error) {
	return scanGiftOrder(s.pool.QueryRow(ctx, selectGiftOrder, senderID, key))
}

func (s *Store) ReserveGift(ctx context.Context, id int64, order gift.Order) (gift.Order, bool, error) {
	var result gift.Order
	var created bool
	err := s.change(ctx, id, func(change *roomChange) error {
		var err error
		result, err = scanGiftOrder(change.tx.QueryRow(ctx, selectGiftOrder, order.Input.SenderID, order.Key))
		if err == nil {
			if result.Input != order.Input {
				return gift.ErrOrderConflict
			}
			return nil
		}
		if !errors.Is(err, gift.ErrOrderNotFound) {
			return err
		}
		liveID, err := change.memberLiveID(order.Input.SenderID)
		if err != nil {
			return err
		}
		if order.Input.LiveID != liveID {
			return live.ErrRoomClosed
		}
		var ownerID int64
		var admin bool
		err = change.tx.QueryRow(ctx, `SELECT owner_id, EXISTS(SELECT 1 FROM room_admins WHERE room_id=$1 AND user_id=$2)
			FROM rooms WHERE id=$1`, id, order.Input.SenderID).Scan(&ownerID, &admin)
		if err != nil {
			return err
		}
		if order.Input.ReceiverID != ownerID || order.Input.SenderID == ownerID {
			return live.ErrForbidden
		}
		if admin {
			order.SenderRole = 4
		}
		result, err = scanGiftOrder(change.tx.QueryRow(ctx, `INSERT INTO gift_orders
			(room_id,live_id,sender_id,receiver_id,request_key,payment_order,request,item,sender_info,receiver_info,sender_role,cost,income)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
			ON CONFLICT(sender_id,request_key) DO NOTHING RETURNING `+giftOrderColumns,
			id, liveID, order.Input.SenderID, ownerID, order.Key, order.PaymentOrder, order.Input, order.Item,
			order.Sender, order.Receiver, order.SenderRole, order.Cost, order.Income))
		if err == nil {
			created = true
			return nil
		}
		if !errors.Is(err, gift.ErrOrderNotFound) {
			return err
		}
		result, err = scanGiftOrder(change.tx.QueryRow(ctx, selectGiftOrder, order.Input.SenderID, order.Key))
		if err == nil && result.Input != order.Input {
			return gift.ErrOrderConflict
		}
		return err
	})
	return result, created, err
}

func (s *Store) FinishGift(ctx context.Context, roomID int64, order gift.Order, receipt payment.Receipt, declined bool) (gift.Order, error) {
	var result gift.Order
	err := s.change(ctx, roomID, func(change *roomChange) error {
		var err error
		result, err = scanGiftOrder(change.tx.QueryRow(ctx, selectGiftOrder+" FOR UPDATE", order.Input.SenderID, order.Key))
		if err != nil || result.State != gift.Pending {
			return err
		}
		if order.ID != result.ID {
			return gift.ErrOrderNotFound
		}
		if declined {
			result.State = gift.Declined
			_, err = change.tx.Exec(ctx, `UPDATE gift_orders SET state=$2 WHERE id=$1`, result.ID, result.State)
			return err
		}
		var providerID *int64
		if result.Cost > 0 {
			if receipt.OrderID <= 0 || receipt.TotalPoints != result.Cost {
				return gift.ErrPending
			}
			providerID = &receipt.OrderID
		}
		result.State, result.Receipt, result.PaidAt = gift.Paid, receipt, time.Now().UTC()
		result.Result, err = result.BuildResult()
		if err != nil {
			return err
		}
		_, err = change.tx.Exec(ctx, `UPDATE gift_orders SET state=$2,paid_at=$3,provider_order_id=$4,receipt=$5,result=$6 WHERE id=$1`,
			result.ID, result.State, result.PaidAt, providerID, result.Receipt, result.Result)
		if err != nil {
			return err
		}
		var online bool
		if err = change.tx.QueryRow(ctx, `SELECT online AND live_id=$2 FROM rooms WHERE id=$1`, roomID, result.Input.LiveID).Scan(&online); err != nil {
			return err
		}
		if online {
			change.event = live.Event{Type: "gift", UserID: result.Input.SenderID, MessageID: result.ID, CreatedAt: result.PaidAt}
			return change.queueBotGift(result.Input.SenderID, result.Cost, result.Item.GiftType)
		}
		return nil
	})
	return result, err
}

func (s *Store) GiftIncome(ctx context.Context, roomID, liveID int64) ([]gift.Income, error) {
	rows, err := s.pool.Query(ctx, `SELECT paid_at,(request->>'gift_id')::bigint,item->>'name',item->>'icon',income,sender_info->>'name',sender_id
		FROM gift_orders WHERE room_id=$1 AND live_id=$2 AND state=1 ORDER BY paid_at DESC,id DESC`, roomID, liveID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (gift.Income, error) {
		var line gift.Income
		err := row.Scan(&line.GivenAt, &line.GiftID, &line.GiftName, &line.GiftIcon, &line.Points, &line.SenderName, &line.SenderID)
		line.GivenAt = line.GivenAt.UTC()
		return line, err
	})
}

func (s *Store) GiftRank(ctx context.Context, anchorID int64, since time.Time) ([]gift.Rank, error) {
	rows, err := s.pool.Query(ctx, `SELECT sender_id,sum(income)::bigint AS total FROM gift_orders
		WHERE receiver_id=$1 AND paid_at>$2 AND state=1 AND income>0 GROUP BY sender_id ORDER BY total DESC,sender_id LIMIT 100`, anchorID, since)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (gift.Rank, error) {
		var rank gift.Rank
		err := row.Scan(&rank.UserID, &rank.Income)
		return rank, err
	})
}

func (s *Store) GiftTotalIncome(ctx context.Context, roomID int64) (*int64, error) {
	var total *int64
	err := s.pool.QueryRow(ctx, `SELECT sum(income)::bigint FROM gift_orders WHERE room_id=$1 AND state=1`, roomID).Scan(&total)
	return total, err
}
