package postgres

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/catalog"
)

func (s *Store) CatalogRows(ctx context.Context, section string) ([]catalog.Row, error) {
	var rows []catalog.Row
	err := s.pool.QueryRow(ctx, "SELECT records FROM public_catalog WHERE section=$1", section).Scan(&rows)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	return rows, err
}
func (s *Store) ReplacePublicCatalog(ctx context.Context, snapshot catalog.Snapshot) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		for section, records := range snapshot {
			if _, err := tx.Exec(ctx, `INSERT INTO public_catalog(section,records) VALUES($1,$2) ON CONFLICT(section) DO UPDATE SET records=excluded.records`, section, records); err != nil {
				return err
			}
		}
		return nil
	})
}
