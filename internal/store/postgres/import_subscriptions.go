package postgres

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"time"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/subscription"
)

type LegacySubscriptionPlan struct {
	subscription.Plan
}

type LegacySubscriptionMember struct {
	subscription.Relationship
	PromotionExpiresAt time.Time
}

type LegacySubscriptionSettlement struct {
	ID, FanID, AnchorID            int64
	No, Subject                    string
	Type, Status                   int
	SettleLD, Points               int64
	TransactionID                  int64
	TransactionNo                  string
	BadDebtPoints, BadDebtLD       int64
	SettleTime, UpdatedAt          *time.Time
	ActualSettleLD                 int64
	SettleConfigID, VerifiedPlanID *int64
	SettleRatio                    string
	CreatedAt                      time.Time
}

type SubscriptionImportCounts struct {
	Plans, Memberships, Settlements, LinkedSettlements int
	Existing, Skipped                                  int
}

func (s *Store) ImportLegacySubscriptions(ctx context.Context, plans iter.Seq2[LegacySubscriptionPlan, error],
	members iter.Seq2[LegacySubscriptionMember, error], settlements iter.Seq2[LegacySubscriptionSettlement, error]) (SubscriptionImportCounts, error) {
	var counts SubscriptionImportCounts
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `LOCK TABLE subscription_plans,subscription_members,legacy_subscription_settlements IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return err
		}
		for plan, err := range plans {
			if err != nil {
				return err
			}
			if err := importLegacySubscriptionPlan(ctx, tx, plan, &counts); err != nil {
				return fmt.Errorf("import subscription plan %d: %w", plan.ID, err)
			}
		}
		for member, err := range members {
			if err != nil {
				return err
			}
			if err := importLegacySubscriptionMember(ctx, tx, member, &counts); err != nil {
				return fmt.Errorf("import subscription member %d for plan %d: %w", member.FanID, member.PlanID, err)
			}
		}
		for settlement, err := range settlements {
			if err != nil {
				return err
			}
			if err := importLegacySubscriptionSettlement(ctx, tx, settlement, &counts); err != nil {
				return fmt.Errorf("import subscription settlement %d: %w", settlement.ID, err)
			}
		}
		_, err := tx.Exec(ctx, `SELECT setval('subscription_plans_id_seq', highest.id, true)
			FROM (SELECT max(id) AS id FROM subscription_plans) highest, subscription_plans_id_seq seq
			WHERE highest.id>seq.last_value OR (highest.id=seq.last_value AND NOT seq.is_called)`)
		return err
	})
	if err != nil {
		return SubscriptionImportCounts{}, err
	}
	return counts, nil
}

func importLegacySubscriptionPlan(ctx context.Context, tx pgx.Tx, row LegacySubscriptionPlan, counts *SubscriptionImportCounts) error {
	if row.ID < 1 || row.AnchorID < 1 || row.Type < subscription.Free || row.Type > subscription.Advanced || row.Price < 0 ||
		row.CreatedAt.IsZero() || row.UpdatedAt.IsZero() {
		return errors.New("invalid source plan identity, price or timestamp")
	}
	var nativeFree bool
	if row.Type == subscription.Free {
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM subscription_plans WHERE anchor_id=$1 AND plan_type=2 AND NOT livemedia_imported)`, row.AnchorID).Scan(&nativeFree); err != nil {
			return err
		}
		if nativeFree {
			return errors.New("target anchor already has a locally created free plan")
		}
	}
	tag, err := tx.Exec(ctx, `INSERT INTO subscription_plans(id,anchor_id,plan_type,published,deleted,price,name,url,remark,created_at,updated_at,
		bundles,legacy_promotion,livemedia_imported)
		SELECT $1,u.id,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,TRUE FROM users u WHERE u.id=$2 ON CONFLICT(id) DO NOTHING`,
		row.ID, row.AnchorID, row.Type, row.Published != 0, row.Deleted != 0, row.Price, row.Name, row.URL, row.Remark, row.CreatedAt,
		row.UpdatedAt, row.Bundle, row.Promote)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		counts.Plans++
		return nil
	}
	var same bool
	err = tx.QueryRow(ctx, `SELECT livemedia_imported AND anchor_id=$2 AND plan_type=$3 FROM subscription_plans WHERE id=$1`,
		row.ID, row.AnchorID, row.Type).Scan(&same)
	return recordLegacySubscriptionResult(err, same, counts)
}

func importLegacySubscriptionMember(ctx context.Context, tx pgx.Tx, row LegacySubscriptionMember, counts *SubscriptionImportCounts) error {
	if row.ID == "" || row.FanID < 1 || row.AnchorID < 1 || row.PlanID < 1 || row.Level < 0 || row.Level > 3 ||
		row.CreatedAt.IsZero() || row.ExpiresAt.IsZero() || row.Reason < 0 || row.Reason > 127 {
		return errors.New("invalid source entitlement identity, state or timestamp")
	}
	if row.FanID == row.AnchorID {
		counts.Skipped++
		return nil
	}
	first := 1
	if !row.PromotionExpiresAt.Equal(row.CreatedAt) {
		first = 2
	}
	tag, err := tx.Exec(ctx, `INSERT INTO subscription_members(fan_id,anchor_id,plan_id,created_at,expires_at,cancelled_at,reason,
		promotion_redeemed,legacy_id,legacy_level,legacy_first,bundle,renewal_authorized)
		SELECT u.id,p.anchor_id,p.id,$4,$5,$6,$7,$8,$9,$10,$11,$12,FALSE FROM subscription_plans p,users u
		WHERE u.id=$1 AND p.anchor_id=$2 AND p.id=$3 AND p.livemedia_imported ON CONFLICT(fan_id,plan_id) DO NOTHING`,
		row.FanID, row.AnchorID, row.PlanID, row.CreatedAt, row.ExpiresAt, row.CancelledAt, row.Reason, row.PromotionPurchased != 0,
		row.ID, row.Level, first, row.Bundle)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		counts.Memberships++
		return nil
	}
	var same bool
	err = tx.QueryRow(ctx, `SELECT COALESCE(legacy_id=$3 AND anchor_id=$4,FALSE) FROM subscription_members WHERE fan_id=$1 AND plan_id=$2`,
		row.FanID, row.PlanID, row.ID, row.AnchorID).Scan(&same)
	return recordLegacySubscriptionResult(err, same, counts)
}

func importLegacySubscriptionSettlement(ctx context.Context, tx pgx.Tx, row LegacySubscriptionSettlement, counts *SubscriptionImportCounts) error {
	if row.ID < 1 || row.FanID < 1 || row.AnchorID < 1 || row.CreatedAt.IsZero() || (row.Type != 6 && row.Type != 7) {
		return errors.New("invalid source settlement identity, type or timestamp")
	}
	var linked *int64
	err := tx.QueryRow(ctx, `INSERT INTO legacy_subscription_settlements(id,no,fan_id,anchor_id,settle_type,settle_ld,expend_point,
		point_transaction_id,point_transaction_no,subject,bad_debt_point,bad_debt_ld,status,settle_time,actual_settle_ld,settle_config_id,
		settle_ratio,create_time,update_time,verified_plan_id)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,NULLIF($17,'')::numeric,$18,$19,
			(SELECT id FROM subscription_plans WHERE id=$20 AND anchor_id=$4 AND livemedia_imported AND plan_type IN (3,4)))
		ON CONFLICT (id) DO NOTHING RETURNING verified_plan_id`, row.ID, row.No, row.FanID, row.AnchorID, row.Type, row.SettleLD, row.Points,
		row.TransactionID, row.TransactionNo, row.Subject, row.BadDebtPoints, row.BadDebtLD, row.Status, row.SettleTime, row.ActualSettleLD, row.SettleConfigID,
		row.SettleRatio, row.CreatedAt, row.UpdatedAt, row.VerifiedPlanID).Scan(&linked)
	if err == nil {
		counts.Settlements++
		if linked != nil {
			counts.LinkedSettlements++
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var same bool
	err = tx.QueryRow(ctx, `SELECT no=$2 AND fan_id=$3 AND anchor_id=$4 AND point_transaction_id=$5 AND point_transaction_no=$6
		FROM legacy_subscription_settlements WHERE id=$1`, row.ID, row.No, row.FanID, row.AnchorID, row.TransactionID, row.TransactionNo).Scan(&same)
	return recordLegacySubscriptionResult(err, same, counts)
}

func recordLegacySubscriptionResult(err error, same bool, counts *SubscriptionImportCounts) error {
	if errors.Is(err, pgx.ErrNoRows) {
		counts.Skipped++
		return nil
	}
	if err != nil {
		return err
	}
	if !same {
		return errors.New("target record conflicts with source identity")
	}
	counts.Existing++
	return nil
}
