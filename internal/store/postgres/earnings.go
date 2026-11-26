package postgres

import (
	"context"
	"iter"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/gift"
)

var _ gift.EarningsStore = (*Store)(nil)

const earningsRows = `WITH entries AS (
	SELECT g.id, g.receiver_id AS anchor_id, g.sender_id AS giver_id, COALESCE(g.sender_info->>'name','') AS display_name,
		2 AS income_type, g.income AS points, g.paid_at AS created_at, COALESCE(g.item->>'name','') AS description
	FROM gift_orders g WHERE g.receiver_id=$1 AND g.state=1 AND ($2::smallint IS NULL OR $2=2)
		AND g.paid_at >= $3 AND g.paid_at <= $4
	UNION ALL
	SELECT o.provider_id,p.owner_id,o.actor_id,COALESCE(u.account,''),o.kind+2,o.points,o.paid_at,
		CASE o.kind WHEN 1 THEN '解锁帖子' ELSE '打赏帖子' END
	FROM anchor_post_orders o JOIN anchor_posts p ON p.id=o.post_id LEFT JOIN users u ON u.id=o.actor_id
	WHERE p.owner_id=$1 AND o.state=1 AND ($2::smallint IS NULL OR $2=o.kind+2)
		AND o.paid_at >= $3 AND o.paid_at <= $4
	UNION ALL
	SELECT l.id, l.anchor_id, l.giver_id, COALESCE(u.account,''), l.income_type, l.points, l.created_at, l.description
	FROM legacy_settlements l LEFT JOIN users u ON u.id=l.giver_id
	WHERE l.anchor_id=$1 AND ($2::smallint IS NULL OR l.income_type=$2 OR ($2 IN (6,7) AND l.income_type IN (6,7)))
		AND l.created_at >= $3 AND l.created_at <= $4
)`

func (s *Store) EarningsSummary(ctx context.Context, f gift.EarningsFilter) ([]gift.EarningsBucket, error) {
	layout := "YYYY-MM-DD"
	if f.Monthly {
		layout = "YYYY-MM"
	}
	rows, err := s.pool.Query(ctx, earningsRows+`
		SELECT to_char(created_at AT TIME ZONE 'UTC', $5), SUM(points)::bigint FROM entries
		GROUP BY 1 ORDER BY 1 DESC`, f.AnchorID, f.Type, f.From, f.To, layout)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (gift.EarningsBucket, error) {
		bucket := gift.EarningsBucket{}
		err := row.Scan(&bucket.Date, &bucket.Points)
		return bucket, err
	})
}

func (s *Store) EarningsDetails(ctx context.Context, f gift.EarningsFilter, page, size int64) (result []gift.EarningsLine, current, count int64, err error) {
	current = page
	err = pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, earningsRows+" SELECT COUNT(*) FROM entries WHERE created_at > $3",
			f.AnchorID, f.Type, f.From, f.To).Scan(&count)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, earningsRows+`
			SELECT id, anchor_id, giver_id, display_name, income_type, points, created_at, description FROM entries
			WHERE created_at > $3 ORDER BY created_at DESC,id DESC LIMIT $5 OFFSET $6`,
			f.AnchorID, f.Type, f.From, f.To, size, (current-1)*size)
		if err != nil {
			return err
		}
		result, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (gift.EarningsLine, error) {
			var line gift.EarningsLine
			err := row.Scan(&line.ID, &line.AnchorID, &line.GiverID, &line.GiverName, &line.Type, &line.Points, &line.CreatedAt, &line.Description)
			line.CreatedAt = line.CreatedAt.UTC()
			return line, err
		})
		return err
	})
	return result, current, count, err
}

func (s *Store) ImportLegacySettlements(ctx context.Context, lines iter.Seq2[gift.EarningsLine, error]) (int64, error) {
	var count int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		for line, err := range lines {
			if err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `INSERT INTO legacy_settlements(id,anchor_id,giver_id,income_type,points,description,created_at)
				VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(id) DO NOTHING`, line.ID, line.AnchorID, line.GiverID,
				line.Type, line.Points, line.Description, line.CreatedAt)
			if err != nil {
				return err
			}
			count += tag.RowsAffected()
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}
