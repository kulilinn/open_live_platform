package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"open_live_platform/internal/payment"
	"open_live_platform/internal/posts"
)

var _ posts.OrderStore = (*Store)(nil)

const anchorPostOrderColumns = `order_no,post_id,actor_id,kind,points,COALESCE(request_key,''),state`

func scanPostOrder(row pgx.Row) (posts.Order, error) {
	var order posts.Order
	err := row.Scan(&order.Number, &order.PostID, &order.ActorID, &order.Kind, &order.Points, &order.Key, &order.State)
	return order, err
}

func (s *Store) ReservePostOrder(ctx context.Context, request posts.Order) (posts.Order, error) {
	var order posts.Order
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		owner, full, status, err := postAccess(ctx, tx, request.ActorID, request.PostID, true)
		if err != nil {
			return err
		}
		if owner == request.ActorID || status != posts.Approved {
			return posts.ErrForbidden
		}
		switch request.Kind {
		case posts.Unlock:
			var level int
			err := tx.QueryRow(ctx, `SELECT visibility,price FROM anchor_posts WHERE id=$1`, request.PostID).Scan(&level, &request.Points)
			if err != nil {
				return err
			}
			if level != 0 || request.Points < 1 {
				return posts.ErrForbidden
			}
		case posts.Tip:
			if !full {
				return posts.ErrForbidden
			}
		default:
			return posts.ErrInvalid
		}
		if request.Key != "" {
			order, err = scanPostOrder(tx.QueryRow(ctx, `SELECT `+anchorPostOrderColumns+` FROM anchor_post_orders
				WHERE actor_id=$1 AND request_key=$2`, request.ActorID, request.Key))
			if err == nil {
				if !samePostOrder(order, request) {
					return posts.ErrPaymentConflict
				}
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		order, err = scanPostOrder(tx.QueryRow(ctx, `SELECT `+anchorPostOrderColumns+` FROM anchor_post_orders
			WHERE post_id=$1 AND actor_id=$2 AND kind=$3
			  AND (state=0 OR (state=1 AND (kind=1 OR ($5='' AND points=$4 AND request_key IS NULL AND created_at>=now()-interval '1 minute'))))
			ORDER BY state,created_at DESC LIMIT 1`, request.PostID, request.ActorID, request.Kind, request.Points, request.Key))
		if err == nil {
			if !samePostOrder(order, request) {
				return posts.ErrPaymentConflict
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		order, err = scanPostOrder(tx.QueryRow(ctx, `INSERT INTO anchor_post_orders (order_no,post_id,actor_id,kind,points,request_key)
			VALUES($1,$2,$3,$4,$5,NULLIF($6,'')) RETURNING `+anchorPostOrderColumns,
			request.Number, request.PostID, request.ActorID, request.Kind, request.Points, request.Key))
		var dbErr *pgconn.PgError
		if errors.As(err, &dbErr) && dbErr.ConstraintName == "anchor_post_order_request" {
			return posts.ErrPaymentConflict
		}
		order.New = err == nil
		return err
	})
	return order, err
}

func samePostOrder(existing, request posts.Order) bool {
	return existing.ActorID == request.ActorID && existing.PostID == request.PostID && existing.Kind == request.Kind &&
		existing.Points == request.Points && (existing.Kind == posts.Unlock || existing.Key == request.Key)
}

func (s *Store) CompletePostOrder(ctx context.Context, expected posts.Order, receipt *payment.Receipt) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		order, err := scanPostOrder(tx.QueryRow(ctx, `SELECT `+anchorPostOrderColumns+` FROM anchor_post_orders WHERE order_no=$1 FOR UPDATE`, expected.Number))
		if err != nil {
			return err
		}
		if !samePostOrder(order, expected) || order.State != posts.OrderPending {
			return posts.ErrPaymentConflict
		}
		if receipt == nil {
			_, err = tx.Exec(ctx, `UPDATE anchor_post_orders SET state=2 WHERE order_no=$1`, expected.Number)
			return err
		}
		if receipt.OrderID < 1 || receipt.OrderNo == "" || receipt.TotalPoints != expected.Points || receipt.BadDebt < 0 || receipt.BadDebt > expected.Points {
			return posts.ErrPaymentConflict
		}
		_, err = tx.Exec(ctx, `UPDATE anchor_post_orders SET state=1,provider_id=$2,provider_no=$3,provider_bad_debt=$4,paid_at=now()
			WHERE order_no=$1`, order.Number, receipt.OrderID, receipt.OrderNo, receipt.BadDebt)
		if err == nil && order.Kind == posts.Tip {
			_, err = tx.Exec(ctx, `UPDATE anchor_posts SET tip_points=tip_points+$2 WHERE id=$1`, order.PostID, order.Points)
		}
		return err
	})
}
