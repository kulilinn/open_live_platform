package postgres

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/anchor"
	"open_live_platform/internal/dm"
	"open_live_platform/internal/subscription"
)

func (s *Store) ReservePurchase(ctx context.Context, fanID, anchorID, planID int64, orderNo string) (subscription.Purchase, error) {
	return s.reserveSubscriptionOrder(ctx, fanID, anchorID, planID, orderNo, nil)
}

func (s *Store) ReserveRenewal(ctx context.Context, candidate subscription.RenewalCandidate, orderNo string) (subscription.Purchase, error) {
	return s.reserveSubscriptionOrder(ctx, candidate.FanID, candidate.AnchorID, candidate.PlanID, orderNo, &candidate.ExpiresAt)
}

func (s *Store) reserveSubscriptionOrder(ctx context.Context, fanID, anchorID, planID int64, orderNo string, renewalFrom *time.Time) (subscription.Purchase, error) {
	var purchase subscription.Purchase
	if fanID < 1 || anchorID < 1 || fanID == anchorID || planID < 1 || orderNo == "" {
		return purchase, subscription.ErrInvalidPlan
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockSubscriptionPair(ctx, tx, fanID, anchorID); err != nil {
			return err
		}
		purchase = subscription.Purchase{FanID: fanID, AnchorID: anchorID, PlanID: planID}
		var price int64
		err := tx.QueryRow(ctx, `SELECT price FROM subscription_plans WHERE id=$1 AND anchor_id=$2 AND published
			AND NOT deleted AND plan_type IN (3,4) FOR SHARE`, planID, anchorID).Scan(&price)
		if errors.Is(err, pgx.ErrNoRows) {
			return subscription.ErrNotFound
		}
		if err != nil {
			return err
		}
		if price < 10 {
			return subscription.ErrInvalidPlan
		}
		if renewalFrom != nil {
			eligible, err := eligibleRenewal(ctx, tx, fanID, anchorID, planID, price/10, *renewalFrom)
			if err != nil {
				return err
			}
			if !eligible {
				return subscription.ErrRenewalIneligible
			}
		}
		err = tx.QueryRow(ctx, `SELECT order_no,points FROM subscription_orders WHERE fan_id=$1 AND plan_id=$2 AND status=0`, fanID, planID).
			Scan(&purchase.OrderNo, &purchase.Points)
		if err == nil {
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if renewalFrom == nil {
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM subscription_members m JOIN subscription_orders o ON o.fan_id=m.fan_id AND o.plan_id=m.plan_id
				WHERE m.fan_id=$1 AND m.plan_id=$2 AND m.cancelled_at IS NULL AND m.expires_at>now() AND o.status=1)`, fanID, planID).Scan(&purchase.Paid); err != nil {
				return err
			}
			if purchase.Paid {
				return nil
			}
		}
		purchase.OrderNo, purchase.Points, purchase.New = orderNo, price/10, true
		_, err = tx.Exec(ctx, `INSERT INTO subscription_orders(order_no,fan_id,plan_id,points,renewal_from) VALUES($1,$2,$3,$4,$5)`,
			purchase.OrderNo, fanID, planID, purchase.Points, renewalFrom)
		return err
	})
	return purchase, err
}

func (s *Store) DeclinePurchase(ctx context.Context, orderNo string) error {
	_, err := s.pool.Exec(ctx, `UPDATE subscription_orders SET status=2 WHERE order_no=$1 AND status=0`, orderNo)
	return err
}

func (s *Store) ConfirmPurchase(ctx context.Context, orderNo string, members dm.Metadata, charge subscription.Charge) error {
	fanID, anchorID := members.Fan.ID, members.Anchor.ID
	if fanID == anchorID || !members.Anchor.Anchor || charge.OrderID < 1 || charge.OrderNo == "" || charge.TotalPoints < 1 ||
		charge.BadDebtPoints < 0 || charge.BadDebtPoints > charge.TotalPoints {
		return subscription.ErrInvalidPlan
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockSubscriptionPair(ctx, tx, fanID, anchorID); err != nil {
			return err
		}
		var planID, points int64
		var status int
		var prior subscription.Charge
		err := tx.QueryRow(ctx, `SELECT o.plan_id,o.points,o.status,COALESCE(o.provider_order_id,0),COALESCE(o.provider_order_no,''),
			COALESCE(o.provider_points,0),COALESCE(o.provider_bad_debt,0)
			FROM subscription_orders o JOIN subscription_plans p ON p.id=o.plan_id
			WHERE o.order_no=$1 AND o.fan_id=$2 AND p.anchor_id=$3 FOR UPDATE OF o`, orderNo, fanID, anchorID).
			Scan(&planID, &points, &status, &prior.OrderID, &prior.OrderNo, &prior.TotalPoints, &prior.BadDebtPoints)
		if errors.Is(err, pgx.ErrNoRows) {
			return subscription.ErrNotFound
		}
		if err != nil {
			return err
		}
		if status == 1 && prior == charge {
			return nil
		}
		if status != 0 || points != charge.TotalPoints {
			return subscription.ErrPurchaseReview
		}
		var joinedPaid bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM subscription_orders o JOIN subscription_plans p ON p.id=o.plan_id
			WHERE o.fan_id=$1 AND p.anchor_id=$2 AND o.status=1)
			OR EXISTS(SELECT 1 FROM subscription_members m JOIN subscription_plans p ON p.id=m.plan_id
			WHERE m.fan_id=$1 AND m.anchor_id=$2 AND p.plan_type>=3 AND m.legacy_id IS NOT NULL AND NOT m.promotion_redeemed)`,
			fanID, anchorID).Scan(&joinedPaid); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE subscription_orders SET status=1,provider_order_id=$2,provider_order_no=$3,provider_points=$4,
			provider_bad_debt=$5,confirmed_at=now() WHERE order_no=$1`, orderNo, charge.OrderID, charge.OrderNo, charge.TotalPoints, charge.BadDebtPoints); err != nil {
			return err
		}
		var start time.Time
		if err := tx.QueryRow(ctx, `SELECT greatest(now(),(SELECT expires_at FROM subscription_members WHERE fan_id=$1 AND plan_id=$2))`,
			fanID, planID).Scan(&start); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO subscription_members(fan_id,anchor_id,plan_id,expires_at) VALUES($1,$2,$3,$4)
			ON CONFLICT (fan_id,plan_id) DO UPDATE SET expires_at=EXCLUDED.expires_at,cancelled_at=NULL,reason=0,
				legacy_level=NULL,renewal_authorized=TRUE`,
			fanID, anchorID, planID, subscription.PaidExpiry(start.UTC())); err != nil {
			return err
		}
		if !joinedPaid {
			if err := recordAnchorTask(ctx, tx, anchorID, time.Now().UTC(), taskEvent{target: anchor.TaskPaidFan, key: strconv.FormatInt(fanID, 10), count: 1}); err != nil {
				return err
			}
		}
		return syncSubscriptionDM(ctx, tx, members)
	})
}
