package postgres

import (
	"context"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/admin"
)

func (s *Store) MCNAgency(ctx context.Context, agency int64) (admin.MCNAgency, error) {
	var data admin.MCNAgency
	err := s.pool.QueryRow(ctx, `SELECT
		(SELECT name FROM mcn_agency_directory WHERE id=$1 AND NOT disabled),
		count(*) FILTER(WHERE status IN (5,6)),count(*) FILTER(WHERE status=6),count(*),
		(SELECT count(DISTINCT m.fan_id) FROM subscription_members m JOIN anchor_profiles a ON a.user_id=m.anchor_id
		 WHERE a.agency_id=$1 AND m.cancelled_at IS NULL AND m.expires_at>=statement_timestamp()),
		(SELECT count(*) FROM backoffice_system_accounts WHERE area='mcn' AND scope=$2 AND account_type=2 AND NOT deleted AND NOT disabled),
		(SELECT count(*) FROM backoffice_system_accounts WHERE area='mcn' AND scope=$2 AND account_type=1 AND NOT deleted AND NOT disabled)
		FROM backoffice_mcn_streamers WHERE mcn_id=$1 AND NOT superseded`, agency, strconv.FormatInt(agency, 10)).
		Scan(&data.Name, &data.StreamerTotal, &data.StreamerCount, &data.RosterCount, &data.FanCount, &data.ManagerCount, &data.AdminCount)
	return data, err
}

func (s *Store) MCNApplicationStatus(ctx context.Context, agency, streamer int64, manager *int64) (int, error) {
	var status int
	err := s.pool.QueryRow(ctx, `SELECT status FROM backoffice_mcn_streamers WHERE mcn_id=$1 AND streamer_id=$2
		AND NOT superseded AND ($3::bigint IS NULL OR manager_id=$3)`, agency, streamer, manager).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		err = admin.ErrBackofficeNotFound
	}
	return status, err
}

func (s *Store) MCNMessages(ctx context.Context, agency int64, manager *int64) ([]admin.MCNMessage, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,ty,user_id,params::text,created_at FROM mcn_messages
		WHERE agency_id=$1 AND ($2::bigint IS NULL OR manager_id=$2) ORDER BY created_at DESC,id DESC LIMIT 50`, agency, manager)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (admin.MCNMessage, error) {
		var message admin.MCNMessage
		err := row.Scan(&message.ID, &message.Type, &message.UserID, &message.Params, &message.CreatedAt)
		message.CreatedAt = message.CreatedAt.UTC()
		return message, err
	})
}

const mcnReportEntries = `WITH records AS (
	SELECT j.streamer_id,j.manager_id,CASE WHEN length(j.create_time)=7 THEN (j.create_time||'-01')::date ELSE j.create_time::date END AS day,j.ld,j.viewer_num AS viewers,j.new_fans_num AS fans,
		nullif(to_timestamp(j.live_start_time),to_timestamp(0)) AS started,
		nullif(to_timestamp(j.live_end_time),to_timestamp(0)) AS ended,h.record->>'live_id' AS live_key,TRUE AS archived
	FROM legacy_business_history h CROSS JOIN LATERAL jsonb_to_record(h.record) j
		(streamer_id bigint,manager_id bigint,create_time text,ld bigint,viewer_num bigint,new_fans_num bigint,live_start_time bigint,live_end_time bigint)
	WHERE h.source_table='mcn_settle_statistic' AND h.owner_id=$1
		AND ($2::bigint IS NULL OR j.manager_id=$2)
		AND (COALESCE(cardinality($3::bigint[]),0)=0 OR j.streamer_id=ANY($3))
		AND CASE WHEN $6::bool THEN (($4::timestamptz IS NULL OR to_timestamp(j.live_start_time)>=$4)
			AND ($5::timestamptz IS NULL OR to_timestamp(j.live_end_time)<$5))
			ELSE (($4::timestamptz IS NULL OR h.record->>'create_time'>=to_char($4 AT TIME ZONE 'UTC','YYYY-MM-DD'))
			AND ($5::timestamptz IS NULL OR h.record->>'create_time'<=to_char($5 AT TIME ZONE 'UTC','YYYY-MM-DD'))) END
	UNION ALL
	SELECT m.streamer_id,m.manager_id,(b.started_at AT TIME ZONE 'UTC')::date AS day,
		COALESCE(payments.ld,0),b.max_audience,subscriptions.fans,b.started_at,b.ended_at,b.live_id::text,FALSE
	FROM backoffice_mcn_streamers m JOIN rooms room ON room.owner_id=m.streamer_id
	JOIN room_broadcast_metrics b ON b.room_id=room.id
	LEFT JOIN mcn_final_settlements done ON done.membership_id=m.id AND done.joined_at=m.joined_at
	LEFT JOIN LATERAL (
		SELECT sum(e.amount)::bigint AS ld FROM (
			SELECT g.income AS amount FROM room_gift_income g WHERE g.live_id=b.live_id AND g.room_id=b.room_id AND g.receiver_id=m.streamer_id
				AND g.paid_at>=b.started_at AND g.paid_at<b.ended_at
			UNION ALL SELECT (d.order_data->>'Income')::bigint FROM date_gift_orders d WHERE d.anchor_id=m.streamer_id AND d.state=1
				AND d.paid_at>=b.started_at AND d.paid_at<b.ended_at AND (d.order_data->>'Cost')::bigint>0
			UNION ALL SELECT o.points FROM anchor_post_orders o JOIN anchor_posts p ON p.id=o.post_id
				WHERE p.owner_id=m.streamer_id AND o.state=1 AND o.paid_at>=b.started_at AND o.paid_at<b.ended_at
			UNION ALL SELECT o.points FROM subscription_orders o JOIN subscription_plans p ON p.id=o.plan_id
				WHERE p.anchor_id=m.streamer_id AND o.status=1 AND o.confirmed_at>=b.started_at AND o.confirmed_at<b.ended_at
		) e
	) payments ON TRUE
	LEFT JOIN LATERAL (
		SELECT count(*) AS fans FROM (SELECT min(created_at) AS first FROM subscription_members
			WHERE anchor_id=m.streamer_id GROUP BY fan_id) sm WHERE sm.first>=b.started_at AND sm.first<b.ended_at
	) subscriptions ON TRUE
	WHERE m.mcn_id=$1 AND ($2::bigint IS NULL OR m.manager_id=$2)
		AND (COALESCE(cardinality($3::bigint[]),0)=0 OR m.streamer_id=ANY($3))
		AND NOT m.superseded AND m.status IN (6,7) AND b.ended_at IS NOT NULL
		AND b.started_at>=m.joined_at AND (m.status=6 OR b.ended_at<=done.confirmed_at)
		AND b.legacy_income IS NULL
		AND NOT EXISTS(SELECT 1 FROM legacy_business_history h WHERE h.source_table='mcn_settle_statistic'
			AND h.owner_id=m.mcn_id AND h.record->>'live_id'=b.live_id::text)
		AND CASE WHEN $6::bool THEN (($4::timestamptz IS NULL OR b.started_at>=$4) AND ($5::timestamptz IS NULL OR b.ended_at<$5))
			ELSE (($4::timestamptz IS NULL OR b.started_at>=$4) AND ($5::timestamptz IS NULL OR b.started_at<$5::timestamptz+interval '24 hours')) END
)`

func (s *Store) MCNPerformance(ctx context.Context, q admin.MCNDateQuery) (admin.MCNPerformance, error) {
	var data admin.MCNPerformance
	err := s.pool.QueryRow(ctx, mcnReportEntries+`SELECT sum(ld)::bigint,sum(viewers)::bigint,sum(fans)::bigint,count(*) FROM records`,
		q.AgencyID, q.ManagerID, q.Streamers, q.Start, q.End, false).
		Scan(&data.LD, &data.ViewerCount, &data.NewFans, &data.StreamerCount)
	return data, err
}

func (s *Store) MCNLiveChart(ctx context.Context, q admin.MCNDateQuery) ([]admin.MCNChartRow, error) {
	rows, err := s.pool.Query(ctx, mcnReportEntries+`
		SELECT to_char(day,'YYYY-MM-DD'),sum(ld)::bigint,sum(viewers)::bigint FROM records GROUP BY day ORDER BY day`,
		q.AgencyID, q.ManagerID, q.Streamers, q.Start, q.End, false)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (admin.MCNChartRow, error) {
		var entry admin.MCNChartRow
		err := row.Scan(&entry.Day, &entry.LD, &entry.ViewerCount)
		return entry, err
	})
}

func (s *Store) MCNStreamerPerformance(ctx context.Context, q admin.MCNDateQuery) ([]admin.MCNStreamerPerformance, error) {
	rows, err := s.pool.Query(ctx, mcnReportEntries+`, totals AS (
		SELECT streamer_id,sum(ld)::bigint AS ld,sum(viewers)::bigint AS viewers,sum(fans)::bigint AS fans,min(started) AS started,max(ended) AS ended
		FROM records GROUP BY streamer_id
	) SELECT t.ld,t.viewers,t.fans,t.started,t.ended,t.streamer_id,m.name,m.manager_name
		FROM totals t JOIN backoffice_mcn_streamers m ON m.mcn_id=$1 AND m.streamer_id=t.streamer_id
		WHERE NOT m.superseded AND ($2::bigint IS NULL OR m.manager_id=$2) ORDER BY t.started DESC,t.streamer_id`,
		q.AgencyID, q.ManagerID, q.Streamers, q.Start, q.End, true)
	if err != nil {
		return nil, err
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (admin.MCNStreamerPerformance, error) {
		var entry admin.MCNStreamerPerformance
		err := row.Scan(&entry.Income, &entry.ViewerCount, &entry.NewFans, &entry.Start, &entry.End, &entry.StreamerID, &entry.StreamerName, &entry.ManagerName)
		return entry, err
	})
	if err == nil && len(items) == 0 {
		err = admin.ErrBackofficeNotFound
	}
	return items, err
}
