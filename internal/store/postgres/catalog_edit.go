package postgres

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/admin"
	"open_live_platform/internal/catalog"
)

func (s *Store) OperateCatalog(ctx context.Context, actor admin.Principal, op admin.CatalogOperation, edit func(catalog.Snapshot, int64) (any, error)) (any, error) {
	var result any
	opts := pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
	if op.Write {
		opts = pgx.TxOptions{}
	}
	err := pgx.BeginTxFunc(ctx, s.pool, opts, func(tx pgx.Tx) error {
		if op.Write {
			if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(167884962,77)"); err != nil {
				return err
			}
		}
		data := catalog.Snapshot{}
		rows, err := tx.Query(ctx, "SELECT section,records FROM public_catalog WHERE section=ANY($1)", op.Sections)
		if err != nil {
			return err
		}
		for rows.Next() {
			var section string
			var records []catalog.Row
			if err := rows.Scan(&section, &records); err != nil {
				rows.Close()
				return err
			}
			data[section] = records
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		var id int64
		if op.Sections[0] == "bad_words" && strings.HasSuffix(op.Path, "/add") {
			if err := tx.QueryRow(ctx, `SELECT setval('word_list_ids', GREATEST(nextval('word_list_ids'),
				COALESCE((SELECT max((r->>'id')::bigint)+1 FROM public_catalog,jsonb_array_elements(records) r WHERE section='bad_words'),1)))`).Scan(&id); err != nil {
				return err
			}
		}
		if strings.HasSuffix(op.Path, "/add") && (op.Sections[0] == "classify" || op.Sections[0] == "questions" || op.Sections[0] == "marquee") {
			if err := tx.QueryRow(ctx, `SELECT setval('editorial_ids',GREATEST(nextval('editorial_ids'),COALESCE((SELECT max((r->>'id')::bigint)+1 FROM public_catalog p,jsonb_array_elements(p.records) r WHERE p.section IN ('classify','questions','marquee')),1)))`).Scan(&id); err != nil {
				return err
			}
		}
		result, err = edit(data, id)
		if err != nil || !op.Write {
			return err
		}
		for _, section := range op.Sections {
			records := data[section]
			if records == nil {
				continue
			}
			if _, err := tx.Exec(ctx, `INSERT INTO public_catalog(section,records) VALUES($1,$2) ON CONFLICT(section) DO UPDATE SET records=excluded.records`, section, records); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `INSERT INTO backoffice_logs(area,tenant,operator_id,operator_name,path,action) VALUES($1,$2,$3,$4,$5,'update_catalog')`, actor.Area, actor.Scope(), actor.ID, actor.Name(), op.Path)
		return err
	})
	return result, err
}
