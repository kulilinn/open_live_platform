package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"open_live_platform/internal/admin"
	"open_live_platform/internal/gift"
)

func (s *Store) GiftTemplates(ctx context.Context) (gift.Interactions, error) {
	return s.GiftPromotionContent(ctx, 0, gift.Interactions{})
}

func (s *Store) SaveGiftPromotion(ctx context.Context, p gift.Promotion, action string) error {
	switch action {
	case "add":
		_, err := s.pool.Exec(ctx, `INSERT INTO gift_promotions(id,gift_id,name,description,starts_at,ends_at) VALUES($1,$2,$3,$4,$5,$6)`,
			p.ID, p.GiftID, p.Name, p.Description, p.Starts, p.Ends)
		return giftTemplateWriteError(err)
	case "edit":
		result, err := s.pool.Exec(ctx, `UPDATE gift_promotions SET gift_id=$2,name=$3,description=$4,starts_at=$5,ends_at=$6
			WHERE id=$1 AND NOT deleted`, p.ID, p.GiftID, p.Name, p.Description, p.Starts, p.Ends)
		if err == nil && result.RowsAffected() == 0 {
			return admin.ErrBackofficeNotFound
		}
		return err
	case "delete":
		_, err := s.pool.Exec(ctx, `UPDATE gift_promotions SET deleted=true WHERE id=$1 AND NOT deleted`, p.ID)
		return err
	default:
		return gift.ErrUnsupported
	}
}

func (s *Store) SaveGiftLimit(ctx context.Context, l gift.Limit, action string) error {
	switch action {
	case "update_status":
		_, err := s.pool.Exec(ctx, `UPDATE gift_purchase_limits SET listing=$2 WHERE id=$1 AND NOT deleted`, l.ID, l.Listing)
		return err
	case "delete":
		_, err := s.pool.Exec(ctx, `UPDATE gift_purchase_limits SET deleted=true WHERE id=$1 AND NOT deleted`, l.ID)
		return err
	case "add", "edit":
		return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			if l.PromotionID > 0 {
				var id int64
				err := tx.QueryRow(ctx, `SELECT id FROM gift_promotions WHERE id=$1 AND gift_id=$2 AND NOT deleted FOR SHARE`, l.PromotionID, l.GiftID).Scan(&id)
				if errors.Is(err, pgx.ErrNoRows) {
					return admin.ErrBackofficeNotFound
				}
				if err != nil {
					return err
				}
			}
			if action == "add" {
				return insertGiftLimit(ctx, tx, l)
			}
			var oldGiftID int64
			err := tx.QueryRow(ctx, `SELECT gift_id FROM gift_purchase_limits WHERE id=$1 AND NOT deleted FOR UPDATE`, l.ID).Scan(&oldGiftID)
			if errors.Is(err, pgx.ErrNoRows) {
				return admin.ErrBackofficeNotFound
			}
			if err != nil {
				return err
			}
			if oldGiftID != l.GiftID {
				return fmt.Errorf("%w: use a new limit ID for a different gift", gift.ErrLimit)
			}
			_, err = tx.Exec(ctx, `UPDATE gift_purchase_limits SET promotion_id=$2,starts_at=$3,ends_at=$4,price=$5,income=$6,
				discount_type=$7,discount_value=$8,per_user=$9,platform=$10,listing=$11 WHERE id=$1`, l.ID,
				l.PromotionID, l.Starts, l.Ends, l.Price, l.Income, l.DiscountType, l.Discount, l.PerUser, l.Platform, l.Listing)
			return err
		})
	default:
		return gift.ErrUnsupported
	}
}

func insertGiftLimit(ctx context.Context, tx pgx.Tx, l gift.Limit) error {
	if l.ID <= 0 || l.GiftID <= 0 || l.PromotionID < 0 || l.Price < 0 || l.Income == nil || *l.Income < 0 || *l.Income > l.Price ||
		l.PerUser < 0 || l.Platform < 0 || l.Listing < 0 || l.Listing > 1 || !l.Starts.Before(l.Ends) {
		return fmt.Errorf("%w: invalid product limit %d", gift.ErrLimit, l.ID)
	}
	_, err := tx.Exec(ctx, `INSERT INTO gift_purchase_limits(id,gift_id,promotion_id,starts_at,ends_at,price,income,
		discount_type,discount_value,per_user,platform,listing,legacy_user_usage,legacy_platform_usage)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'{}'::jsonb,0)`, l.ID, l.GiftID,
		l.PromotionID, l.Starts, l.Ends, l.Price, l.Income, l.DiscountType, l.Discount, l.PerUser, l.Platform, l.Listing)
	return giftTemplateWriteError(err)
}

func giftTemplateWriteError(err error) error {
	var dbErr *pgconn.PgError
	if errors.As(err, &dbErr) && dbErr.Code == "23505" {
		return admin.ErrGiftTemplateExists
	}
	return err
}
