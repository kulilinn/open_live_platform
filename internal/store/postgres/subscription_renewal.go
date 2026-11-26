package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/dm"
	"open_live_platform/internal/subscription"
)

const lastSubscriptionPoints = `(SELECT o.points FROM subscription_orders o WHERE o.fan_id=m.fan_id AND o.plan_id=m.plan_id
	AND o.status=1 ORDER BY o.confirmed_at DESC LIMIT 1)`

func (s *Store) RenewalCandidates(ctx context.Context, cutoff time.Time) ([]subscription.RenewalCandidate, error) {
	rows, err := s.pool.Query(ctx, `SELECT m.fan_id,m.anchor_id,m.plan_id,m.expires_at FROM subscription_members m
		JOIN subscription_plans p ON p.id=m.plan_id JOIN users u ON u.id=m.fan_id
		WHERE m.renewal_authorized AND m.cancelled_at IS NULL AND m.expires_at>now() AND m.expires_at<$1 AND p.published AND NOT p.deleted
			AND p.plan_type IN (3,4) AND p.price/10=`+lastSubscriptionPoints+` AND NOT (u.account_status=2 AND `+activeAccountLimit+`)
			AND NOT EXISTS(SELECT 1 FROM users u WHERE u.id=m.anchor_id AND u.account_status=2 AND `+activeAccountLimit+`)
			AND NOT EXISTS(SELECT 1 FROM subscription_orders o WHERE o.fan_id=m.fan_id AND o.plan_id=m.plan_id AND o.status=0)
		ORDER BY m.expires_at,m.fan_id,m.plan_id LIMIT 100`, cutoff)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[subscription.RenewalCandidate])
}

func eligibleRenewal(ctx context.Context, tx pgx.Tx, fanID, anchorID, planID, points int64, from time.Time) (bool, error) {
	var eligible bool
	err := tx.QueryRow(ctx, `SELECT COALESCE($4=`+lastSubscriptionPoints+`,FALSE) FROM subscription_members m JOIN users u ON u.id=m.fan_id
		WHERE m.fan_id=$1 AND m.anchor_id=$2 AND m.plan_id=$3 AND m.renewal_authorized AND m.cancelled_at IS NULL AND m.expires_at=$5 AND m.expires_at>now()
		AND NOT (u.account_status=2 AND `+activeAccountLimit+`)
		AND NOT EXISTS(SELECT 1 FROM users u WHERE u.id=m.anchor_id AND u.account_status=2 AND `+activeAccountLimit+`)`, fanID, anchorID, planID, points, from).Scan(&eligible)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return eligible, err
}

func (s *Store) DeclineRenewal(ctx context.Context, orderNo string, members dm.Metadata) error {
	fanID, anchorID := members.Fan.ID, members.Anchor.ID
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockSubscriptionPair(ctx, tx, fanID, anchorID); err != nil {
			return err
		}
		var planID int64
		var from time.Time
		err := tx.QueryRow(ctx, `UPDATE subscription_orders o SET status=2 FROM subscription_plans p
			WHERE o.order_no=$1 AND o.fan_id=$2 AND p.anchor_id=$3 AND p.id=o.plan_id AND o.status=0 AND o.renewal_from IS NOT NULL
			RETURNING o.plan_id,o.renewal_from`, orderNo, fanID, anchorID).Scan(&planID, &from)
		if errors.Is(err, pgx.ErrNoRows) {
			return subscription.ErrRenewalIneligible
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE subscription_members SET cancelled_at=now(),reason=7
			WHERE fan_id=$1 AND anchor_id=$2 AND plan_id=$3 AND expires_at=$4 AND cancelled_at IS NULL`, fanID, anchorID, planID, from); err != nil {
			return err
		}
		return syncSubscriptionDM(ctx, tx, members)
	})
}
