package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/gift"
)

func (s *Store) ImportGiftPromotionContent(ctx context.Context, content gift.Interactions) error {
	if len(content.Promotions) == 0 && len(content.Limits) == 0 && len(content.Boxes) == 0 && len(content.QuickGifts) == 0 {
		return nil
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		for _, p := range content.Promotions {
			if p.ID <= 0 || p.GiftID <= 0 || !p.Starts.Before(p.Ends) {
				return fmt.Errorf("invalid gift promotion %d", p.ID)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO gift_promotions(id,gift_id,name,description,starts_at,ends_at)
				VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, p.ID, p.GiftID, p.Name, p.Description, p.Starts, p.Ends); err != nil {
				return err
			}
		}
		for _, l := range content.Limits {
			if _, err := tx.Exec(ctx, `INSERT INTO gift_purchase_limits(id,gift_id,promotion_id,starts_at,ends_at,price,income,
				discount_type,discount_value,per_user,platform,listing,legacy_user_usage,legacy_platform_usage)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) ON CONFLICT DO NOTHING`, l.ID, l.GiftID,
				l.PromotionID, l.Starts, l.Ends, l.Price, l.Income, l.DiscountType, l.Discount, l.PerUser, l.Platform, l.Listing, l.UserUsage, l.PlatformUsage); err != nil {
				return err
			}
		}
		return importGiftCollections(ctx, tx, content)
	})
}

const giftLimitColumns = `l.id,l.gift_id,l.promotion_id,l.starts_at,l.ends_at,l.price,l.income,l.discount_type,
	l.discount_value,l.per_user,l.platform,l.listing`

func giftLimitFields(item *gift.Limit) []any {
	return []any{&item.ID, &item.GiftID, &item.PromotionID, &item.Starts, &item.Ends, &item.Price,
		&item.Income, &item.DiscountType, &item.Discount, &item.PerUser, &item.Platform, &item.Listing}
}

func (s *Store) GiftLimitQuote(ctx context.Context, input gift.Input, item gift.Gift) (int64, int64, error) {
	var limit gift.Limit
	err := s.pool.QueryRow(ctx, `SELECT `+giftLimitColumns+` FROM gift_purchase_limits l
		WHERE id=$1 AND gift_id=$2 AND NOT deleted AND listing=1 AND starts_at<=statement_timestamp() AND ends_at>statement_timestamp()`,
		input.LimitID, input.GiftID).Scan(giftLimitFields(&limit)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, fmt.Errorf("%w: unavailable benefit", gift.ErrLimit)
	}
	if err != nil {
		return 0, 0, err
	}
	return limit.Cost(item, input.Quantity)
}

type giftLimitUsage struct {
	rule            gift.Limit
	legacyPlatform  *int64
	legacyUserKnown bool
	legacyUser      int64
	platform, user  int64
}

func (r *roomChange) lockGiftLimit(order gift.Order, reserve bool) (giftLimitUsage, error) {
	var usage giftLimitUsage
	availability := ""
	if reserve {
		availability = ` AND NOT l.deleted AND l.listing=1 AND l.starts_at<=statement_timestamp() AND l.ends_at>statement_timestamp()`
	}
	// Acquire the lock before taking a snapshot of orders from other rooms.
	err := r.tx.QueryRow(r.ctx, `SELECT `+giftLimitColumns+`,l.legacy_platform_usage,COALESCE(jsonb_typeof(l.legacy_user_usage)='object',false),
		COALESCE((l.legacy_user_usage->>$3)::bigint,0) FROM gift_purchase_limits l WHERE l.id=$1 AND l.gift_id=$2`+availability+` FOR UPDATE`,
		order.Input.LimitID, order.Input.GiftID, strconv.FormatInt(order.Input.SenderID, 10)).
		Scan(append(giftLimitFields(&usage.rule), &usage.legacyPlatform, &usage.legacyUserKnown, &usage.legacyUser)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return usage, fmt.Errorf("%w: unavailable benefit", gift.ErrLimit)
	}
	if err != nil {
		return usage, err
	}
	err = r.tx.QueryRow(r.ctx, `SELECT count(*),count(*) FILTER (WHERE sender_id=$2) FROM gift_orders
		WHERE purchase_limit_id=$1 AND state IN (0,1) AND ($3::boolean OR state=1)`, order.Input.LimitID, order.Input.SenderID, reserve).
		Scan(&usage.platform, &usage.user)
	return usage, err
}

func (r *roomChange) reserveGiftLimit(order gift.Order) error {
	u, err := r.lockGiftLimit(order, true)
	if err != nil {
		return err
	}
	cost, income, err := u.rule.Cost(order.Item, order.Input.Quantity)
	if err != nil {
		return err
	}
	if cost != order.Cost || income != order.Income {
		return fmt.Errorf("%w: the promotion has changed", gift.ErrLimit)
	}
	quantity := int64(order.Input.Quantity)
	if u.rule.Platform > 0 && (u.legacyPlatform == nil || quantity > u.rule.Platform || u.platform >= u.rule.Platform-*u.legacyPlatform) {
		return fmt.Errorf("%w: platform limit:%d", gift.ErrLimit, u.rule.Platform)
	}
	if u.rule.PerUser > 0 && (!u.legacyUserKnown || quantity > u.rule.PerUser || u.user >= u.rule.PerUser-u.legacyUser) {
		return fmt.Errorf("%w: person limit:%d", gift.ErrLimit, u.rule.PerUser)
	}
	return nil
}

func (r *roomChange) giftLimitReached(order gift.Order) (bool, error) {
	u, err := r.lockGiftLimit(order, false)
	if err != nil {
		return false, err
	}
	platformReached := u.rule.Platform > 0 && u.legacyPlatform != nil && u.platform >= u.rule.Platform-*u.legacyPlatform-1
	userReached := u.rule.PerUser > 0 && u.legacyUserKnown && u.user >= u.rule.PerUser-u.legacyUser-1
	return platformReached || userReached, nil
}

func (s *Store) GiftPromotionContent(ctx context.Context, userID int64, content gift.Interactions) (gift.Interactions, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,gift_id,name,description,starts_at,ends_at FROM gift_promotions WHERE NOT deleted ORDER BY id`)
	if err != nil {
		return content, err
	}
	content.Promotions, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (gift.Promotion, error) {
		var p gift.Promotion
		err := row.Scan(&p.ID, &p.GiftID, &p.Name, &p.Description, &p.Starts, &p.Ends)
		return p, err
	})
	if err != nil {
		return content, err
	}
	rows, err = s.pool.Query(ctx, `SELECT `+giftLimitColumns+`,l.legacy_platform_usage+usage.platform,
		CASE WHEN jsonb_typeof(l.legacy_user_usage)='object' THEN COALESCE((l.legacy_user_usage->>$2)::bigint,0)+usage.actor END
		FROM gift_purchase_limits l CROSS JOIN LATERAL (
			SELECT count(*) AS platform,count(*) FILTER(WHERE sender_id=$1) AS actor FROM gift_orders
			WHERE purchase_limit_id=l.id AND state IN (0,1)
		) usage WHERE NOT l.deleted ORDER BY l.id`, userID, strconv.FormatInt(userID, 10))
	if err != nil {
		return content, err
	}
	content.Limits, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (gift.Limit, error) {
		var l gift.Limit
		var used *int64
		err := row.Scan(append(giftLimitFields(&l), &l.PlatformUsage, &used)...)
		if used != nil {
			l.UserUsage = map[int64]int64{userID: *used}
		}
		return l, err
	})
	if err != nil {
		return content, err
	}
	return s.readGiftCollections(ctx, content)
}
