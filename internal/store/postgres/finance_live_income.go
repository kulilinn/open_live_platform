package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/admin"
)

func (s *Store) BackofficeLiveIncome(ctx context.Context, q admin.LiveIncomeQuery) ([]admin.LiveIncomeRow, int64, error) {
	const relation = `FROM (
		WITH archived AS (
			SELECT h.source_id AS key,a.* FROM legacy_business_history h CROSS JOIN LATERAL jsonb_to_record(h.record) a
			(streamer_sid text,streamer_uid bigint,streamer_name text,mcn_id bigint,mcn_name text,live_start_time timestamptz,
			live_end_time timestamptz,live_duration bigint,diamond numeric,create_at date)
			WHERE h.source_table='anchor_live_income_statistic' AND a.live_start_time BETWEEN $1 AND $2
		)
		SELECT * FROM archived
		UNION ALL
		SELECT f.live_id::text,f.streamer_sid,f.streamer_id,f.streamer_name,f.mcn_id,f.mcn_name,f.started_at,f.ended_at,f.minutes,f.diamond,f.report_day
		FROM finance_live_income f WHERE f.started_at BETWEEN $1 AND $2
		AND NOT EXISTS(SELECT 1 FROM archived a WHERE a.streamer_uid=f.streamer_id AND a.live_start_time=f.started_at AND a.create_at=f.report_day)
	) report WHERE ($3::bigint IS NULL OR streamer_uid=$3) AND ($4::text IS NULL OR streamer_sid=$4)
		AND ($5::text IS NULL OR streamer_name=$5) AND ($6::bigint IS NULL OR mcn_id=$6) AND ($7::text IS NULL OR mcn_name=$7)`
	const columns = `streamer_sid,streamer_uid,streamer_name,mcn_id,mcn_name,live_start_time,live_end_time,live_duration,
		diamond::text,to_char(create_at,'YYYY-MM-DD')`
	args := []any{q.Start.UTC(), q.End.UTC(), q.Streamer, q.SearchID, q.Name, q.MCN, q.MCNName}
	return readBackofficePage(ctx, s, q.PageRequest, columns, relation, `create_at DESC,live_start_time DESC,key DESC`, args,
		func(row pgx.CollectableRow) (admin.LiveIncomeRow, error) {
			var r admin.LiveIncomeRow
			err := row.Scan(&r.SearchID, &r.Streamer, &r.Name, &r.MCN, &r.MCNName, &r.Start, &r.End, &r.Minutes, &r.Diamond, &r.Created)
			if r.Start != nil {
				utc := r.Start.UTC()
				r.Start = &utc
			}
			if r.End != nil {
				utc := r.End.UTC()
				r.End = &utc
			}
			return r, err
		})
}

func (s *Store) CollectLiveIncome(ctx context.Context, before time.Time) error {
	_, err := s.pool.Exec(ctx, `WITH sources AS (
		SELECT o.live_id,o.room_id,o.receiver_id,o.income,o.cost,o.sender_id,o.provider_order_id,o.receipt
		FROM gift_orders o WHERE o.state=1 AND o.item->>'effectControlType' IS DISTINCT FROM '7' AND o.paid_at >= $1 AND o.paid_at < $2
		UNION ALL
		SELECT o.live_id,o.room_id,o.receiver_id,a.income,o.cost,o.sender_id,o.provider_order_id,o.receipt
		FROM rocket_refuel_awards a JOIN gift_orders o ON o.id=a.order_id AND o.state=1 WHERE a.settled_at >= $1 AND a.settled_at < $2
		UNION ALL
		SELECT w.live_id,w.room_id,w.anchor_id,(w.order_data->>'Price')::bigint,(w.order_data->>'Price')::bigint,w.user_id,w.provider_id,w.order_data->'Receipt'
		FROM room_wheel_orders w WHERE w.state=1 AND w.paid_at >= $1 AND w.paid_at < $2
	), settled AS (
		SELECT p.*,COALESCE(verified.bad_debt_points,(p.receipt->>'total_bad_debt_point')::bigint,0) AS debt
		FROM sources p LEFT JOIN finance_bad_debt_receipts verified ON verified.provider_order_id=p.provider_order_id
		AND verified.provider_order_no=p.receipt->>'order_no' AND verified.charged_points=p.cost AND verified.spender_id=p.sender_id
		AND p.receipt->>'order_id'=p.provider_order_id::text AND (p.receipt->>'total_point')::bigint=p.cost
	), daily AS (
		SELECT b.live_id,b.started_at,b.ended_at,p.receiver_id,
			sum(p.income-COALESCE(trunc(p.income::numeric*p.debt/NULLIF(p.cost,0)),0)) AS diamond
		FROM room_broadcast_metrics b JOIN settled p ON p.live_id=b.live_id AND p.room_id=b.room_id
		WHERE b.legacy_income IS NULL GROUP BY b.live_id,p.receiver_id
	)
	INSERT INTO finance_live_income(live_id,streamer_id,report_day,streamer_sid,streamer_name,mcn_id,mcn_name,started_at,ended_at,minutes,diamond)
	SELECT d.live_id,u.id,($2::timestamptz AT TIME ZONE 'UTC')::date,COALESCE(u.profile->>'SearchName',u.account),u.account,m.id,m.name,d.started_at,d.ended_at,
		COALESCE(trunc(extract(epoch FROM d.ended_at-d.started_at)/60),0)::bigint,d.diamond
	FROM daily d JOIN users u ON u.id=d.receiver_id
	LEFT JOIN anchor_profiles ap ON ap.user_id=u.id LEFT JOIN mcn_agency_directory m ON m.id=ap.agency_id
	ON CONFLICT (report_day,live_id,streamer_id) DO UPDATE SET ended_at=EXCLUDED.ended_at,minutes=EXCLUDED.minutes,diamond=EXCLUDED.diamond`,
		before.AddDate(0, 0, -1), before)
	return err
}
