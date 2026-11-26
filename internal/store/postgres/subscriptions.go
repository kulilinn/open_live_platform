package postgres

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"open_live_platform/internal/anchor"
	"open_live_platform/internal/dm"
	"open_live_platform/internal/subscription"
)

var _ subscription.Store = (*Store)(nil)

const subscriptionPlanColumns = `p.id,p.anchor_id,p.plan_type,p.published::int,p.deleted::int,p.price,p.name,p.url,p.remark,p.created_at,p.updated_at,
	p.bundles,p.promotion,p.promotion_ends_at`

func (s *Store) EnsureFreePlan(ctx context.Context, anchorID int64) error {
	return ensureFreePlan(ctx, s.pool, anchorID)
}

func ensureFreePlan(ctx context.Context, executor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, anchorID int64) error {
	_, err := executor.Exec(ctx, `INSERT INTO subscription_plans(anchor_id,plan_type,price) SELECT $1,2,0
		WHERE NOT EXISTS(SELECT 1 FROM subscription_plans WHERE anchor_id=$1 AND plan_type=2) ON CONFLICT DO NOTHING`, anchorID)
	return err
}

func (s *Store) CreatePlan(ctx context.Context, anchorID int64, planType int, input subscription.PlanInput) (subscription.Plan, error) {
	return scanSubscriptionPlan(s.pool.QueryRow(ctx, `INSERT INTO subscription_plans AS p (anchor_id,plan_type,price,name,url,remark)
		VALUES($1,$2,$3,$4,$5,$6) RETURNING `+subscriptionPlanColumns+`,0`, anchorID, planType, input.Price, input.Name, input.URL, input.Remark))
}

func scanSubscriptionPlan(row pgx.Row) (subscription.Plan, error) {
	var plan subscription.Plan
	err := row.Scan(&plan.ID, &plan.AnchorID, &plan.Type, &plan.Published, &plan.Deleted, &plan.Price, &plan.Name,
		&plan.URL, &plan.Remark, &plan.CreatedAt, &plan.UpdatedAt, &plan.Bundle, &plan.Promote, &plan.PromotionEndsAt, &plan.Count)
	plan.CreatedAt, plan.UpdatedAt = plan.CreatedAt.UTC(), plan.UpdatedAt.UTC()
	return plan, err
}

func (s *Store) ChangePlan(ctx context.Context, anchorID, planID int64, change subscription.PlanChange) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		plan, err := scanSubscriptionPlan(tx.QueryRow(ctx, `SELECT `+subscriptionPlanColumns+`,0
			FROM subscription_plans p WHERE p.id=$1 AND p.anchor_id=$2 AND NOT p.deleted FOR UPDATE`, planID, anchorID))
		if errors.Is(err, pgx.ErrNoRows) {
			return subscription.ErrNotFound
		}
		if err != nil {
			return err
		}
		if fields := change.Fields; fields != nil {
			if plan.Type == subscription.Free {
				return subscription.ErrInvalidPlan
			}
			plan.Price, plan.Name, plan.URL, plan.Remark = fields.Price, fields.Name, fields.URL, fields.Remark
		}
		published := plan.Published == 1
		if change.Published != nil {
			published = *change.Published
		}
		if err := plan.ApplyOffers(change, time.Now()); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE subscription_plans SET price=$2,name=$3,url=$4,remark=$5,published=$6,deleted=$7,updated_at=now(),
			bundles=$8,promotion=$9,promotion_ends_at=$10 WHERE id=$1`, plan.ID, plan.Price, plan.Name, plan.URL, plan.Remark, published,
			change.Delete, plan.Bundle, plan.Promote, plan.PromotionEndsAt)
		return err
	})
}

func (s *Store) SetFreeSubscriptionOpen(ctx context.Context, anchorID int64, open bool) error {
	tag, err := s.pool.Exec(ctx, `UPDATE subscription_plans SET published=$2,updated_at=now() WHERE anchor_id=$1 AND plan_type=2 AND NOT deleted`, anchorID, open)
	if err == nil && tag.RowsAffected() == 0 {
		return subscription.ErrNotFound
	}
	return err
}

func (s *Store) Plans(ctx context.Context, anchorID int64, includeHidden bool) ([]subscription.Plan, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+subscriptionPlanColumns+`,(SELECT count(*) FROM subscription_members m
		WHERE m.plan_id=p.id AND m.cancelled_at IS NULL AND m.expires_at>now())
		FROM subscription_plans p WHERE p.anchor_id=$1 AND ($2 OR (p.published AND NOT p.deleted)) ORDER BY p.created_at DESC,p.id DESC`, anchorID, includeHidden)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	return pgx.AppendRows([]subscription.Plan{}, rows, func(row pgx.CollectableRow) (subscription.Plan, error) {
		plan, err := scanSubscriptionPlan(row)
		return plan.VisiblePromotion(now), err
	})
}

func (s *Store) CanCreatePlan(ctx context.Context, anchorID int64) (bool, error) {
	var allowed bool
	err := s.pool.QueryRow(ctx, `SELECT NOT EXISTS (SELECT 1 FROM subscription_plans p JOIN subscription_members m ON m.plan_id=p.id
		WHERE p.anchor_id=$1 AND p.deleted AND m.expires_at>now())`, anchorID).Scan(&allowed)
	return allowed, err
}

func (s *Store) Relationships(ctx context.Context, fanID, anchorID int64) ([]subscription.Relationship, error) {
	rows, err := s.pool.Query(ctx, `SELECT m.fan_id,m.anchor_id,m.plan_id,p.name,p.price,COALESCE(m.legacy_level,p.plan_type-2),
		m.created_at,m.expires_at,m.cancelled_at,m.reason,m.promotion_redeemed,now(),COALESCE(m.legacy_id,''),m.bundle,COALESCE(m.legacy_first,0)
		FROM subscription_members m JOIN subscription_plans p ON p.id=m.plan_id
		WHERE ($1::bigint=0 OR m.fan_id=$1) AND ($2::bigint=0 OR m.anchor_id=$2) ORDER BY m.created_at DESC,m.plan_id DESC`, fanID, anchorID)
	if err != nil {
		return nil, err
	}
	return pgx.AppendRows([]subscription.Relationship{}, rows, func(row pgx.CollectableRow) (subscription.Relationship, error) {
		var relationship subscription.Relationship
		var now time.Time
		var promotion bool
		err := row.Scan(&relationship.FanID, &relationship.AnchorID, &relationship.PlanID, &relationship.PlanName, &relationship.PlanPrice, &relationship.Level,
			&relationship.CreatedAt, &relationship.ExpiresAt, &relationship.CancelledAt, &relationship.Reason, &promotion, &now,
			&relationship.ID, &relationship.Bundle, &relationship.First)
		if promotion {
			relationship.PromotionPurchased = 1
			if relationship.First == 0 {
				relationship.First = 2
			}
		}
		if relationship.ID == "" {
			relationship.ID = subscription.RelationshipID(relationship.FanID, relationship.AnchorID, relationship.PlanID)
		}
		relationship.CreatedAt, relationship.ExpiresAt = relationship.CreatedAt.UTC(), relationship.ExpiresAt.UTC()
		if relationship.CancelledAt != nil {
			cancelledAt := relationship.CancelledAt.UTC()
			relationship.CancelledAt = &cancelledAt
		}
		return relationship.View(subscription.Plan{Name: relationship.PlanName, Price: relationship.PlanPrice}, now), err
	})
}

func lockSubscriptionPair(ctx context.Context, tx pgx.Tx, fanID, anchorID int64) error {
	key := "subscription:" + strconv.FormatInt(min(fanID, anchorID), 10) + ":" + strconv.FormatInt(max(fanID, anchorID), 10)
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key)
	return err
}

func syncSubscriptionDM(ctx context.Context, tx pgx.Tx, members dm.Metadata) error {
	var until *time.Time
	err := tx.QueryRow(ctx, `SELECT max(expires_at) FROM subscription_members WHERE cancelled_at IS NULL AND expires_at>now()
		AND ((fan_id=$1 AND anchor_id=$2) OR (fan_id=$2 AND anchor_id=$1))`, members.Fan.ID, members.Anchor.ID).Scan(&until)
	if err != nil {
		return err
	}
	_, err = setDMSubscription(ctx, tx, members, until != nil, members.Welcome, until)
	if until == nil && errors.Is(err, dm.ErrNotFound) {
		return nil
	}
	return err
}

func (s *Store) SubscribeFree(ctx context.Context, members dm.Metadata) error {
	fanID, anchorID := members.Fan.ID, members.Anchor.ID
	if fanID == anchorID || !members.Anchor.Anchor {
		return subscription.ErrForbidden
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockSubscriptionPair(ctx, tx, fanID, anchorID); err != nil {
			return err
		}
		var planID int64
		err := tx.QueryRow(ctx, `SELECT id FROM subscription_plans WHERE anchor_id=$1 AND plan_type=2 AND published AND NOT deleted
			ORDER BY created_at DESC,id DESC LIMIT 1 FOR SHARE`, anchorID).Scan(&planID)
		if errors.Is(err, pgx.ErrNoRows) {
			return subscription.ErrNotFound
		}
		if err != nil {
			return err
		}
		var followed bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM subscription_members m JOIN subscription_plans p ON p.id=m.plan_id
			WHERE m.fan_id=$1 AND m.anchor_id=$2 AND p.plan_type=2)`, fanID, anchorID).Scan(&followed); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO subscription_members(fan_id,anchor_id,plan_id,expires_at) VALUES($1,$2,$3,now()+interval '3650 days')
			ON CONFLICT (fan_id,plan_id) DO UPDATE SET created_at=now(),expires_at=EXCLUDED.expires_at,cancelled_at=NULL,reason=0
			WHERE subscription_members.cancelled_at IS NOT NULL OR subscription_members.expires_at<=now()`, fanID, anchorID, planID)
		if err != nil {
			return err
		}
		if !followed {
			if err := recordAnchorTask(ctx, tx, anchorID, time.Now().UTC(), taskEvent{target: anchor.TaskFollow, key: strconv.FormatInt(fanID, 10), count: 1}); err != nil {
				return err
			}
		}
		return syncSubscriptionDM(ctx, tx, members)
	})
}

func (s *Store) SubscribeTrial(ctx context.Context, members dm.Metadata, planID int64) error {
	fanID, anchorID := members.Fan.ID, members.Anchor.ID
	if fanID == anchorID || !members.Anchor.Anchor {
		return subscription.ErrForbidden
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockSubscriptionPair(ctx, tx, fanID, anchorID); err != nil {
			return err
		}
		plan, err := scanSubscriptionPlan(tx.QueryRow(ctx, `SELECT `+subscriptionPlanColumns+`,0
			FROM subscription_plans p WHERE p.id=$1 AND p.anchor_id=$2 FOR SHARE`, planID, anchorID))
		if errors.Is(err, pgx.ErrNoRows) {
			return subscription.ErrNotFound
		}
		if err != nil {
			return err
		}
		var alreadyClaimed bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM subscription_members WHERE fan_id=$1 AND plan_id=$2)
			OR EXISTS(SELECT 1 FROM subscription_orders WHERE fan_id=$1 AND plan_id=$2 AND status=0)`, fanID, planID).Scan(&alreadyClaimed); err != nil {
			return err
		}
		if alreadyClaimed {
			return subscription.ErrTrialClaimed
		}
		now := time.Now().UTC()
		expires, err := plan.TrialExpiry(now)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO subscription_members(fan_id,anchor_id,plan_id,created_at,expires_at,promotion_redeemed)
			VALUES($1,$2,$3,$4,$5,TRUE)`, fanID, anchorID, planID, now, expires)
		if err != nil {
			return err
		}
		return syncSubscriptionDM(ctx, tx, members)
	})
}

func (s *Store) Cancel(ctx context.Context, members dm.Metadata, planID int64, reason int) error {
	fanID, anchorID := members.Fan.ID, members.Anchor.ID
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockSubscriptionPair(ctx, tx, fanID, anchorID); err != nil {
			return err
		}
		var pending string
		err := tx.QueryRow(ctx, `SELECT o.order_no FROM subscription_orders o JOIN subscription_plans p ON p.id=o.plan_id
			WHERE o.fan_id=$1 AND p.anchor_id=$2 AND o.plan_id=$3 AND o.status=0 AND o.renewal_from IS NOT NULL`, fanID, anchorID, planID).Scan(&pending)
		if err == nil {
			return subscription.PendingRenewal{OrderNo: pending}
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE subscription_members m SET cancelled_at=COALESCE(cancelled_at,now()),
			reason=CASE WHEN cancelled_at IS NULL THEN $4 ELSE reason END,
			expires_at=CASE WHEN cancelled_at IS NULL AND p.plan_type=2 THEN LEAST(expires_at,now()) ELSE expires_at END
			FROM subscription_plans p WHERE p.id=m.plan_id AND m.fan_id=$1 AND m.anchor_id=$2 AND m.plan_id=$3`, fanID, anchorID, planID, reason)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return subscription.ErrNotFound
		}
		return syncSubscriptionDM(ctx, tx, members)
	})
}
