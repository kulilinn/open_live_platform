package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/live"
)

type HistoryRow struct {
	Source, ID string
	Owner      int64
	Record     json.RawMessage
	Password   string
}
type HistoryCounts struct{ Read, Changed, Rooms, Broadcasts, Reports, Stars int64 }

// ImportLegacyHistory saves source snapshots for reconciliation and restores closed room history.
// Spendable balances stay with the configured payment provider.
func (s *Store) ImportLegacyHistory(ctx context.Context, rows iter.Seq2[HistoryRow, error]) (HistoryCounts, error) {
	var c HistoryCounts
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL TIME ZONE 'UTC'; LOCK TABLE legacy_business_history IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return err
		}
		for row, err := range rows {
			if err != nil {
				return err
			}
			if row.Source == "anchor_users" || row.Source == "anchor_users_kyc" {
				n, err := restoreAnchorReview(ctx, tx, row)
				if err != nil {
					return fmt.Errorf("%s %s: %w", row.Source, row.ID, err)
				}
				c.Read++
				c.Changed += n
				continue
			}
			tag, err := tx.Exec(ctx, `INSERT INTO legacy_business_history AS h(source_table,source_id,owner_id,record) VALUES ($1,$2,$3,$4)
    ON CONFLICT (source_table,source_id) DO UPDATE SET owner_id=excluded.owner_id,record=excluded.record
    WHERE (h.owner_id,h.record) IS DISTINCT FROM (excluded.owner_id,excluded.record)`, row.Source, row.ID, row.Owner, row.Record)
			if err != nil {
				return fmt.Errorf("%s %s: %w", row.Source, row.ID, err)
			}
			c.Read++
			c.Changed += tag.RowsAffected()
			if row.Source != "room" {
				continue
			}
			hash, err := live.HashRoomPassword(row.Password)
			if err != nil {
				return err
			}
			tag, err = tx.Exec(ctx, `INSERT INTO rooms(id,owner_id,title,online,anchor_joined,created_at,updated_at,banner,tag,description,offline_info,plan,gifts,
    trans_enabled,trans_lang,password_hash,moderation_until)
    SELECT u.id,u.id,r.subject,false,false,r.create_at,r.create_at,r.banner,r.tag,r.description,r.offline_info,
     COALESCE(NULLIF(r.plan,'')::jsonb,'[]'::jsonb),COALESCE(NULLIF(r.gifts,'')::jsonb,'[]'::jsonb),
     r.trans<>0,r.trans_lang,$4,CASE WHEN r.limited=0 THEN 0 ELSE COALESCE(extract(epoch FROM r.limited_time)::bigint,-1) END
    FROM users u,jsonb_to_record($3) AS r(subject text,create_at timestamptz,banner text,tag text,description text,offline_info text,plan text,
     gifts text,trans int,trans_lang text,limited int,limited_time timestamptz) WHERE u.id=$2 AND u.id::text=$1 ON CONFLICT(id) DO NOTHING`, row.ID, row.Owner, row.Record, hash)
			if err != nil {
				return fmt.Errorf("restore room %s: %w", row.ID, err)
			}
			c.Rooms += tag.RowsAffected()
		}
		metrics, err := tx.Exec(ctx, `INSERT INTO room_broadcast_metrics(live_id,room_id,started_at,ended_at,max_audience,like_total,legacy_income,legacy_new_fans,legacy_gift_fans)
   SELECT h.source_id::bigint,r.id,j.start_time,j.end_time,j.audience,COALESCE(j.like_quantity,0),j.income,j.new_fans,j.gift_fans
   FROM legacy_business_history h JOIN rooms r ON r.id::text=h.record->>'room'
   CROSS JOIN LATERAL jsonb_to_record(h.record) j(start_time timestamptz,end_time timestamptz,audience bigint,like_quantity bigint,income bigint,new_fans bigint,gift_fans bigint)
   WHERE h.source_table='room_metrics' AND j.end_time>j.start_time ON CONFLICT(live_id) DO NOTHING`)
		if err != nil {
			return err
		}
		c.Broadcasts = metrics.RowsAffected()
		reports, err := tx.Exec(ctx, `INSERT INTO room_reports(id,room,reporter_id,subject,description,reported_at) OVERRIDING SYSTEM VALUE
   SELECT h.source_id::bigint,h.record->>'room',u.id,h.record->>'subject',h.record->>'description',(h.record->>'time')::timestamptz
   FROM legacy_business_history h JOIN users u ON u.id=h.owner_id WHERE h.source_table='room_report' ON CONFLICT(id) DO NOTHING`)
		if err != nil {
			return err
		}
		c.Reports = reports.RowsAffected()
		stars, err := tx.Exec(ctx, `WITH entries AS (SELECT h.owner_id,h.source_id::bigint AS id,(h.record->>'balance')::bigint AS stars,
   (h.record->>'create_time')::timestamptz AS ts FROM legacy_business_history h JOIN users u ON u.id=h.owner_id WHERE h.source_table='star_coin_flow_records'),
   latest AS (SELECT DISTINCT ON(owner_id) *,min(ts) OVER(PARTITION BY owner_id) AS first_ts FROM entries ORDER BY owner_id,ts DESC,id DESC)
   INSERT INTO legacy_anchor_star_balances AS b(anchor_id,last_source_id,stars,started_at,observed_at) SELECT owner_id,id,stars,first_ts,ts FROM latest
   ON CONFLICT(anchor_id) DO UPDATE SET last_source_id=excluded.last_source_id,stars=excluded.stars,started_at=least(b.started_at,excluded.started_at),observed_at=excluded.observed_at
   WHERE (excluded.observed_at,excluded.last_source_id)>(b.observed_at,b.last_source_id)`)
		if err != nil {
			return err
		}
		c.Stars = stars.RowsAffected()
		_, err = tx.Exec(ctx, `SELECT setval('room_live_id_seq',m.n,true) FROM (SELECT max(live_id) AS n FROM room_broadcast_metrics) m, room_live_id_seq s WHERE m.n>s.last_value OR (m.n=s.last_value AND NOT s.is_called);
   SELECT setval('room_reports_id_seq',m.n,true) FROM (SELECT max(id) AS n FROM room_reports) m,room_reports_id_seq s WHERE m.n>s.last_value OR (m.n=s.last_value AND NOT s.is_called)`)
		return err
	})
	if err != nil {
		return HistoryCounts{}, err
	}
	return c, nil
}
