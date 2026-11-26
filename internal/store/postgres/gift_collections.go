package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/admin"
	"open_live_platform/internal/gift"
)

func importGiftCollections(ctx context.Context, tx pgx.Tx, content gift.Interactions) error {
	for _, box := range content.Boxes {
		if _, err := tx.Exec(ctx, `INSERT INTO gift_boxes(id,name,limit_ids) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`,
			box.ID, box.Name, box.LimitIDs); err != nil {
			return err
		}
	}
	if len(content.QuickGifts) == 0 {
		return nil
	}
	marker, err := tx.Exec(ctx, `INSERT INTO gift_collection_bootstrap(kind) VALUES('quick_gift_config') ON CONFLICT DO NOTHING`)
	if err != nil || marker.RowsAffected() == 0 {
		return err
	}
	for _, entry := range content.QuickGifts {
		if _, err := tx.Exec(ctx, `INSERT INTO gift_shortcuts(id,limit_id,promotion_id,sort) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`,
			entry.ID, entry.LimitID, entry.PromotionID, entry.Sort); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) readGiftCollections(ctx context.Context, content gift.Interactions) (gift.Interactions, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,name,limit_ids FROM gift_boxes WHERE NOT deleted ORDER BY id`)
	if err != nil {
		return content, err
	}
	content.Boxes, err = pgx.CollectRows(rows, pgx.RowToStructByPos[gift.Box])
	if err != nil {
		return content, err
	}
	rows, err = s.pool.Query(ctx, `SELECT id,limit_id,promotion_id,sort FROM gift_shortcuts WHERE NOT deleted ORDER BY id`)
	if err != nil {
		return content, err
	}
	content.QuickGifts, err = pgx.CollectRows(rows, pgx.RowToStructByPos[gift.QuickGift])
	return content, err
}

func (s *Store) SaveGiftBox(ctx context.Context, box gift.Box, action string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error { return saveGiftBox(ctx, tx, box, action) })
}

func saveGiftBox(ctx context.Context, tx pgx.Tx, box gift.Box, action string) error {
	switch action {
	case "add":
		_, err := tx.Exec(ctx, `INSERT INTO gift_boxes(id,name,limit_ids) VALUES($1,$2,$3)`, box.ID, box.Name, box.LimitIDs)
		return giftTemplateWriteError(err)
	case "edit":
		result, err := tx.Exec(ctx, `UPDATE gift_boxes SET name=$2,limit_ids=$3 WHERE id=$1 AND NOT deleted`, box.ID, box.Name, box.LimitIDs)
		if err == nil && result.RowsAffected() == 0 {
			return admin.ErrBackofficeNotFound
		}
		return err
	case "delete":
		_, err := tx.Exec(ctx, `UPDATE gift_boxes SET deleted=true WHERE id=$1 AND NOT deleted`, box.ID)
		return err
	default:
		return gift.ErrUnsupported
	}
}

func (s *Store) SaveGiftShortcut(ctx context.Context, entry gift.QuickGift, action string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error { return saveGiftShortcut(ctx, tx, entry, action) })
}

func saveGiftShortcut(ctx context.Context, tx pgx.Tx, entry gift.QuickGift, action string) error {
	switch action {
	case "add":
		_, err := tx.Exec(ctx, `INSERT INTO gift_shortcuts(id,limit_id,promotion_id,sort) VALUES($1,$2,$3,$4)`,
			entry.ID, entry.LimitID, entry.PromotionID, entry.Sort)
		return giftTemplateWriteError(err)
	case "edit":
		result, err := tx.Exec(ctx, `UPDATE gift_shortcuts SET limit_id=$2 WHERE id=$1 AND NOT deleted`, entry.ID, entry.LimitID)
		if err == nil && result.RowsAffected() == 0 {
			return admin.ErrBackofficeNotFound
		}
		return err
	case "delete":
		_, err := tx.Exec(ctx, `UPDATE gift_shortcuts SET deleted=true WHERE id=$1 AND NOT deleted`, entry.ID)
		return err
	case "empty":
		if _, err := tx.Exec(ctx, `INSERT INTO gift_collection_bootstrap(kind) VALUES('quick_gift_config') ON CONFLICT DO NOTHING`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM gift_shortcuts`)
		return err
	default:
		return gift.ErrUnsupported
	}
}

func (s *Store) ApplyGiftSheet(ctx context.Context, actor admin.Principal, path, kind string, batch admin.GiftSheetBatch) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var count int
		switch kind {
		case "gift_config":
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(167884962,62)`); err != nil {
				return err
			}
			for _, item := range batch.Gifts {
				if _, err := tx.Exec(ctx, `INSERT INTO backoffice_gifts(id,item,deleted) VALUES($1,$2,false)`, item.ID, item); err != nil {
					return giftTemplateWriteError(err)
				}
			}
			count = len(batch.Gifts)
		case "gift_box":
			for _, box := range batch.Boxes {
				if err := saveGiftBox(ctx, tx, box, "add"); err != nil {
					return err
				}
			}
			count = len(batch.Boxes)
		case "quick_gift_config":
			for _, shortcut := range batch.Shortcuts {
				if err := saveGiftShortcut(ctx, tx, shortcut, "add"); err != nil {
					return err
				}
			}
			count = len(batch.Shortcuts)
		case "product_limit_config":
			for _, limit := range batch.Limits {
				if err := insertGiftLimit(ctx, tx, limit); err != nil {
					return err
				}
			}
			count = len(batch.Limits)
		default:
			return gift.ErrUnsupported
		}
		_, err := tx.Exec(ctx, `INSERT INTO backoffice_logs(area,tenant,operator_id,operator_name,path,target,action)
			VALUES($1,$2,$3,$4,$5,$6,$7)`, actor.Area, actor.Scope(), actor.ID, actor.Name(), path, fmt.Sprint(count), "gift_import_"+kind)
		return err
	})
}
