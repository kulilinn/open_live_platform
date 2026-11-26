package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/upload"
)

func (s *Store) StageUpload(ctx context.Context, p upload.Part, finish upload.Finish) error {
	key := p.Key()
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM uploads WHERE object_key IN
			(SELECT object_key FROM uploads WHERE expires_at<=now() ORDER BY expires_at LIMIT 32 FOR UPDATE SKIP LOCKED)`)
		if err != nil {
			return err
		}
		var count int
		var done bool
		err = tx.QueryRow(ctx, `INSERT INTO uploads(object_key,part_count,expires_at) VALUES($1,$2,now()+interval '30 minutes')
			ON CONFLICT(object_key) DO UPDATE SET expires_at=EXCLUDED.expires_at RETURNING part_count,complete`, key, p.Count).Scan(&count, &done)
		if err != nil {
			return err
		}
		if count != p.Count {
			return upload.ErrConflict
		}
		if done {
			return nil
		}
		tag, err := tx.Exec(ctx, `INSERT INTO upload_parts(object_key,part_index,data) VALUES($1,$2,$3)
			ON CONFLICT(object_key,part_index) DO UPDATE SET part_index=EXCLUDED.part_index WHERE upload_parts.data=EXCLUDED.data`, key, p.Index, p.Data)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return upload.ErrConflict
		}
		var received int
		var size int64
		err = tx.QueryRow(ctx, "SELECT count(*),sum(octet_length(data)) FROM upload_parts WHERE object_key=$1", key).Scan(&received, &size)
		if err != nil {
			return err
		}
		if size > p.Limit {
			return upload.ErrLimit
		}
		if received != count {
			return nil
		}
		rows, err := tx.Query(ctx, "SELECT data FROM upload_parts WHERE object_key=$1 ORDER BY part_index", key)
		if err != nil {
			return err
		}
		err = finish(ctx, key, func(yield func([]byte, error) bool) {
			defer rows.Close()
			for rows.Next() {
				var data []byte
				err := rows.Scan(&data)
				if !yield(data, err) || err != nil {
					return
				}
			}
			if err := rows.Err(); err != nil {
				yield(nil, err)
			}
		})
		rows.Close()
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "DELETE FROM upload_parts WHERE object_key=$1", key)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "UPDATE uploads SET complete=true WHERE object_key=$1", key)
		return err
	})
}
