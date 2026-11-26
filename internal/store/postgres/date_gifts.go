package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/date"
	"open_live_platform/internal/gift"
	"open_live_platform/internal/payment"
)

func readDateGift(row pgx.Row) (date.GiftOrder, error) {
	var order date.GiftOrder
	err := row.Scan(&order)
	if errors.Is(err, pgx.ErrNoRows) {
		err = gift.ErrOrderNotFound
	}
	return order, err
}

func (s *Store) FindDateGift(ctx context.Context, senderID int64, key string) (date.GiftOrder, error) {
	return readDateGift(s.pool.QueryRow(ctx, `SELECT order_data FROM date_gift_orders WHERE sender_id=$1 AND request_key=$2`, senderID, key))
}

func (s *Store) ReserveDateGift(ctx context.Context, candidate date.GiftOrder) (date.GiftOrder, bool, error) {
	var order date.GiftOrder
	var created bool
	err := s.dateChange(ctx, func(tx pgx.Tx) error {
		existing, err := readDateGift(tx.QueryRow(ctx, `SELECT order_data FROM date_gift_orders WHERE sender_id=$1 AND request_key=$2`,
			candidate.Input.Sender, candidate.Key))
		if err == nil {
			if existing.Input != candidate.Input {
				return gift.ErrOrderConflict
			}
			order = existing
			return nil
		}
		if !errors.Is(err, gift.ErrOrderNotFound) {
			return err
		}
		session, err := loadDateSession(ctx, tx, candidate.Input.Channel)
		if err != nil {
			return err
		}
		if session.AnchorID != candidate.Input.Receiver || session.AudienceID != candidate.Input.Sender {
			return date.ErrForbidden
		}
		if !session.Active(time.Now()) {
			return date.ErrClosed
		}
		blocked, err := dateGiftBlocked(ctx, tx, session.Channel, candidate.Item.ID)
		if err != nil {
			return err
		}
		if blocked {
			return gift.ErrBlocked
		}
		if candidate.State == date.GiftWaiting {
			var waiting bool
			err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM date_gift_orders WHERE channel=$1 AND state=3)`, session.Channel).Scan(&waiting)
			if err != nil {
				return err
			}
			if waiting {
				return date.ErrGameWaiting
			}
		}
		_, err = tx.Exec(ctx, `INSERT INTO date_gift_orders(id,channel,anchor_id,sender_id,request_key,state,order_data)
			VALUES($1,$2,$3,$4,$5,$6,$7)`, candidate.ID, session.Channel, session.AnchorID, session.AudienceID, candidate.Key, candidate.State, candidate)
		order, created = candidate, err == nil
		return err
	})
	return order, created, err
}

func (s *Store) DecideDateGift(ctx context.Context, channel, orderID, actor int64, accept bool) (date.GiftOrder, bool, error) {
	var order date.GiftOrder
	var changed bool
	err := s.dateChange(ctx, func(tx pgx.Tx) error {
		session, err := loadDateSession(ctx, tx, channel)
		if err != nil {
			return err
		}
		if session.AnchorID != actor {
			return date.ErrForbidden
		}
		order, err = readDateGift(tx.QueryRow(ctx, `SELECT order_data FROM date_gift_orders WHERE channel=$1 AND id=$2`, channel, orderID))
		if err != nil {
			return err
		}
		if order.Item.GiftType != 1 {
			return gift.ErrOrderNotFound
		}
		if order.State != date.GiftWaiting {
			return nil
		}
		if !session.Active(time.Now()) {
			return date.ErrClosed
		}
		if accept {
			blocked, err := dateGiftBlocked(ctx, tx, channel, order.Item.ID)
			if err != nil {
				return err
			}
			if blocked {
				return gift.ErrBlocked
			}
			order.State = date.GiftPending
		} else {
			order.State = date.GiftRefused
		}
		_, err = tx.Exec(ctx, `UPDATE date_gift_orders SET state=$2,order_data=$3 WHERE id=$1`, order.ID, order.State, order)
		changed = err == nil
		return err
	})
	return order, changed, err
}

func (s *Store) FinishDateGift(ctx context.Context, candidate date.GiftOrder, receipt payment.Receipt, declined bool) (date.GiftOrder, bool, error) {
	var order date.GiftOrder
	var changed bool
	err := s.dateChange(ctx, func(tx pgx.Tx) error {
		var err error
		order, err = readDateGift(tx.QueryRow(ctx, `SELECT order_data FROM date_gift_orders WHERE id=$1`, candidate.ID))
		if err != nil || order.State != date.GiftPending {
			return err
		}
		var providerID *int64
		var paidAt *time.Time
		if declined {
			order.State = date.GiftDeclined
		} else {
			if order.Cost > 0 {
				if receipt.OrderID <= 0 || receipt.TotalPoints != order.Cost {
					return gift.ErrPending
				}
				providerID = &receipt.OrderID
			}
			order.State, order.PaidAt, order.Receipt = date.GiftPaid, time.Now().UTC(), receipt
			paidAt = &order.PaidAt
		}
		_, err = tx.Exec(ctx, `UPDATE date_gift_orders SET state=$2,order_data=$3,provider_id=$4,paid_at=$5 WHERE id=$1`,
			order.ID, order.State, order, providerID, paidAt)
		if err != nil {
			return err
		}
		changed = true
		if order.State != date.GiftPaid || order.Item.GiftType != 2 {
			return nil
		}
		session, err := loadDateSession(ctx, tx, order.Input.Channel)
		if err != nil || !session.Active(order.PaidAt) {
			return err
		}
		var start time.Time
		if err := tx.QueryRow(ctx, `SELECT GREATEST($2,max(end_at)) FROM date_gift_queue WHERE channel=$1`,
			order.Input.Channel, order.PaidAt).Scan(&start); err != nil {
			return err
		}
		for range order.Input.Quantity {
			end := start.Add(time.Duration(order.Duration()) * time.Second)
			if _, err := tx.Exec(ctx, `INSERT INTO date_gift_queue(id,channel,order_id,start_at,end_at) VALUES($1,$2,$3,$4,$5)`,
				date.NewChannel(), order.Input.Channel, order.ID, start, end); err != nil {
				return err
			}
			start = end
		}
		return nil
	})
	return order, changed, err
}

func (s *Store) DateGifts(ctx context.Context, channel int64, state int) ([]date.GiftOrder, error) {
	rows, err := s.pool.Query(ctx, `SELECT order_data FROM date_gift_orders WHERE channel=$1 AND state=$2 ORDER BY paid_at,id`, channel, state)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (date.GiftOrder, error) { return readDateGift(row) })
}

func (s *Store) DateGiftQueue(ctx context.Context, channel int64) ([]date.GiftQueueEntry, error) {
	rows, err := s.pool.Query(ctx, `SELECT q.id,q.start_at,q.end_at,o.order_data
		FROM date_gift_queue q JOIN date_gift_orders o ON o.id=q.order_id
		WHERE q.channel=$1 AND q.end_at>statement_timestamp() ORDER BY q.start_at,q.id`, channel)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanDateGiftQueue)
}

func scanDateGiftQueue(row pgx.CollectableRow) (date.GiftQueueEntry, error) {
	var entry date.GiftQueueEntry
	err := row.Scan(&entry.ID, &entry.Start, &entry.End, &entry.Order)
	return entry, err
}

func (s *Store) ClaimDateGiftStarts(ctx context.Context) ([]date.GiftQueueEntry, error) {
	var entries []date.GiftQueueEntry
	err := s.dateChange(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `WITH due AS (
			SELECT q.id FROM date_gift_queue q JOIN date_sessions s ON s.channel=q.channel
			WHERE q.announced_at IS NULL AND q.start_at<=statement_timestamp() AND q.end_at>statement_timestamp()
			AND s.ended_at IS NULL
			AND (s.state->>'AnchorSeen')::timestamptz+$1::interval>statement_timestamp()
			AND (s.state->>'AudienceSeen')::timestamptz+$1::interval>statement_timestamp()
			ORDER BY q.start_at,q.id LIMIT 64
		), started AS (
			UPDATE date_gift_queue q SET announced_at=statement_timestamp() FROM due WHERE due.id=q.id
			RETURNING q.id,q.start_at,q.end_at,q.order_id
		) SELECT q.id,q.start_at,q.end_at,o.order_data FROM started q JOIN date_gift_orders o ON o.id=q.order_id`, date.ParticipantLease)
		if err != nil {
			return err
		}
		entries, err = pgx.CollectRows(rows, scanDateGiftQueue)
		return err
	})
	return entries, err
}
