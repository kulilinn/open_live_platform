package postgres

import (
	"context"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/admin"
	"open_live_platform/internal/gift"
)

func (s *Store) GiftOverrides(ctx context.Context) ([]gift.CatalogEntry, error) {
	rows, err := s.pool.Query(ctx, `SELECT item,deleted FROM backoffice_gifts`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (gift.CatalogEntry, error) { return scanGiftOverride(row) })
}

func scanGiftOverride(row pgx.Row) (gift.CatalogEntry, error) {
	var entry gift.CatalogEntry
	err := row.Scan(&entry.Item, &entry.Deleted)
	return entry, err
}

func (s *Store) GiftOverride(ctx context.Context, id int64) (gift.CatalogEntry, bool, error) {
	entry, err := scanGiftOverride(s.pool.QueryRow(ctx, `SELECT item,deleted FROM backoffice_gifts WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return entry, false, nil
	}
	return entry, err == nil, err
}

func (s *Store) EditGiftCatalog(ctx context.Context, actor admin.Principal, path, action string, item gift.Gift, baseline bool) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(167884962,62)`); err != nil {
			return err
		}
		previous, err := scanGiftOverride(tx.QueryRow(ctx, `SELECT item,deleted FROM backoffice_gifts WHERE id=$1`, item.ID))
		exists := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if action == "add" {
			if exists || baseline {
				return gift.ErrCatalogConflict
			}
		} else if !exists && !baseline || exists && previous.Deleted && action != "delete" {
			return admin.ErrBackofficeNotFound
		}
		if action == "delete" && exists {
			item = previous.Item
		}
		if _, err := tx.Exec(ctx, `INSERT INTO backoffice_gifts(id,item,deleted) VALUES($1,$2,$3)
		    ON CONFLICT(id) DO UPDATE SET item=EXCLUDED.item,deleted=EXCLUDED.deleted,changed_at=statement_timestamp()`,
			item.ID, item, action == "delete"); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO backoffice_logs(area,tenant,operator_id,operator_name,path,target,action)
			VALUES($1,$2,$3,$4,$5,$6,$7)`, actor.Area, actor.Scope(), actor.ID, actor.Name(), path,
			strconv.FormatInt(item.ID, 10), "gift_"+action)
		return err
	})
}

func verifyGiftCatalogQuote(ctx context.Context, tx pgx.Tx, item gift.Gift) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(167884962,62)`); err != nil {
		return err
	}
	var active bool
	err := tx.QueryRow(ctx, `SELECT NOT deleted AND item=$2::jsonb FROM backoffice_gifts WHERE id=$1`, item.ID, item).Scan(&active)
	if err == nil && !active {
		return gift.ErrCatalogConflict
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}
