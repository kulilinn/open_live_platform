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

func (s *Store) AccessSetting(ctx context.Context, key string) (string, error) {
	var content string
	err := s.pool.QueryRow(ctx, `SELECT coalesce((SELECT item->>'content' FROM public_catalog,jsonb_array_elements(records) item
		WHERE section='common_settings' AND item->>'id'=$1),'')`, key).Scan(&content)
	return content, err
}
func (s *Store) ReplacePublicCatalog(ctx context.Context, snapshot catalog.Snapshot) error {
	return s.importPublicCatalog(ctx, snapshot, func(string) bool { return true })
}

// SeedPublicCatalog imports missing sections and replaces only the named sections.
func (s *Store) SeedPublicCatalog(ctx context.Context, snapshot catalog.Snapshot, replaceSections map[string]bool) error {
	return s.importPublicCatalog(ctx, snapshot, func(section string) bool { return replaceSections[section] })
}

func (s *Store) importPublicCatalog(ctx context.Context, snapshot catalog.Snapshot, replace func(string) bool) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(167884962,77)"); err != nil {
			return err
		}
		for section, records := range snapshot {
			statement := `INSERT INTO public_catalog(section,records) VALUES($1,$2) ON CONFLICT(section) DO NOTHING`
			if replace(section) {
				statement = `INSERT INTO public_catalog(section,records) VALUES($1,$2) ON CONFLICT(section) DO UPDATE SET records=excluded.records`
			}
			result, err := tx.Exec(ctx, statement, section, records)
			if err != nil {
				return err
			}
			if section == "bad_words" && result.RowsAffected() == 0 {
				if err := fillLegacyWordListFiles(ctx, tx, records); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func fillLegacyWordListFiles(ctx context.Context, tx pgx.Tx, records []catalog.Row) error {
	var files []catalog.Row
	for _, row := range records {
		if _, ok := row["file"]; ok {
			files = append(files, row)
		}
	}
	if len(files) == 0 {
		return nil
	}
	// Only fill a missing file when all metadata matches. Keep operator edits and deletes.
	_, err := tx.Exec(ctx, `WITH merged AS (
		SELECT jsonb_agg(coalesce(legacy.item, stored.item) ORDER BY stored.ord) AS records
		FROM public_catalog p
		CROSS JOIN LATERAL jsonb_array_elements(p.records) WITH ORDINALITY AS stored(item, ord)
		LEFT JOIN jsonb_array_elements($1::jsonb) AS legacy(item)
			ON NOT (stored.item ? 'file') AND stored.item=legacy.item-'file'
		WHERE p.section='bad_words'
	)
	UPDATE public_catalog p SET records=merged.records FROM merged
	WHERE p.section='bad_words' AND merged.records IS NOT NULL AND p.records IS DISTINCT FROM merged.records`, files)
	return err
}
