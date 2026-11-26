package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/admin"
)

const mcnIncomeFilters = ` AND h.owner_id=$1 AND ($2::bigint IS NULL OR j.manager_id=$2)
	AND (COALESCE(cardinality($3::bigint[]),0)=0 OR j.streamer_id=ANY($3))
	AND ($4::text IS NULL OR left(j.create_time,length($4))>=$4)
	AND ($5::text IS NULL OR left(j.create_time,length($5))<=$5)`

func (s *Store) MCNStreamerIncome(ctx context.Context, q admin.MCNIncomeQuery) ([]admin.MCNStreamerIncome, int64, error) {
	const relation = `FROM mcn_finance_records h CROSS JOIN LATERAL jsonb_to_record(h.record) j
		(manager_id bigint,streamer_id bigint,ld bigint,star_coin_total bigint,live_flow_total bigint,ratio numeric,
		 live_start_time bigint,live_end_time bigint,create_time text)
		LEFT JOIN backoffice_mcn_streamers m ON m.mcn_id=h.owner_id AND m.streamer_id=j.streamer_id AND NOT m.superseded
		LEFT JOIN legacy_business_history u ON u.source_table='mcn_user' AND u.source_id=j.manager_id::text AND u.owner_id=$1
		WHERE h.source_table='mcn_settle_statistic'` + mcnIncomeFilters
	const columns = `h.owner_id,j.manager_id,COALESCE(u.record->>'name',CASE WHEN m.manager_id=j.manager_id THEN m.manager_name END),
		j.streamer_id,m.name,m.email,j.ld,j.star_coin_total,j.live_flow_total,COALESCE(j.ratio,m.ratio)::text,
		m.manager_ratio::text,m.agency_ratio::text,j.live_start_time,j.live_end_time,j.create_time`
	return readBackofficePage(ctx, s, q.PageRequest, columns, relation, "j.create_time DESC,h.source_id::bigint DESC",
		[]any{q.AgencyID, q.ManagerID, q.Streamers, q.From, q.To}, func(row pgx.CollectableRow) (admin.MCNStreamerIncome, error) {
			var e admin.MCNStreamerIncome
			err := row.Scan(&e.AgencyID, &e.ManagerID, &e.ManagerName, &e.StreamerID, &e.StreamerName, &e.StreamerEmail,
				&e.LD, &e.StarCoinTotal, &e.LiveFlowTotal, &e.Ratio, &e.ManagerRatio, &e.AgencyRatio, &e.LiveStart, &e.LiveEnd, &e.Day)
			return e, err
		})
}

func (s *Store) MCNIncomeSplits(ctx context.Context, q admin.MCNIncomeQuery) ([]admin.MCNIncomeSplit, int64, error) {
	const relation = `FROM mcn_finance_records h CROSS JOIN LATERAL jsonb_to_record(h.record) j
		(manager_id bigint,streamer_id bigint,ld bigint,ratio numeric,manager_ratio numeric,agency_ratio numeric,create_time text)
		LEFT JOIN backoffice_mcn_streamers m ON m.mcn_id=h.owner_id AND m.streamer_id=j.streamer_id AND NOT m.superseded
		LEFT JOIN LATERAL (
			SELECT r.record->>'name' AS name,r.record->>'manager_name' AS manager_name FROM legacy_business_history ur
			JOIN legacy_business_history r ON r.source_table='mcn_role' AND r.owner_id=$1 AND r.source_id=ur.record->>'role_id'
			WHERE ur.source_table='mcn_user_role' AND ur.owner_id=j.manager_id ORDER BY r.source_id::bigint LIMIT 1
		) role ON TRUE
		WHERE h.source_table='mcn_settle_agency_streamer_statistic'` + mcnIncomeFilters
	const columns = `j.manager_id,j.streamer_id,m.name,j.ld,j.ratio::text,j.manager_ratio::text,j.agency_ratio::text,
		j.create_time,role.name,role.manager_name`
	return readBackofficePage(ctx, s, q.PageRequest, columns, relation, "j.create_time DESC,h.source_id::bigint DESC",
		[]any{q.AgencyID, q.ManagerID, q.Streamers, q.From, q.To}, func(row pgx.CollectableRow) (admin.MCNIncomeSplit, error) {
			var e admin.MCNIncomeSplit
			err := row.Scan(&e.ManagerID, &e.StreamerID, &e.StreamerName, &e.LD, &e.Ratio, &e.ManagerRatio, &e.AgencyRatio,
				&e.Day, &e.RoleName, &e.RoleManagerName)
			return e, err
		})
}

func (s *Store) MCNMonthlyIncome(ctx context.Context, agency int64, month string) ([]admin.MCNMonthlyIncome, error) {
	rows, err := s.pool.Query(ctx, `SELECT (record->>'ld')::bigint,(record->>'ratio')::numeric::text
		FROM mcn_finance_records WHERE source_table='mcn_settle_platform_statistic' AND owner_id=$1
		AND record->>'create_time'=$2 ORDER BY source_id::bigint`, agency, month)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (admin.MCNMonthlyIncome, error) {
		var e admin.MCNMonthlyIncome
		err := row.Scan(&e.LD, &e.Ratio)
		return e, err
	})
}

func (s *Store) MCNPlatformRatio(ctx context.Context, q admin.MCNDateQuery) (string, error) {
	var ratio string
	err := s.pool.QueryRow(ctx, mcnReportEntries+`, total AS (SELECT COALESCE(sum(ld),0) AS ld FROM records)
		SELECT h.ratio::text FROM mcn_template_directory h CROSS JOIN total t
		WHERE h.agency_id=0 AND h.template_id=1 AND NOT h.disabled
		AND t.ld BETWEEN h.lower_bound AND h.upper_bound ORDER BY h.lower_bound DESC,h.id LIMIT 1`,
		q.AgencyID, nil, nil, q.Start, q.End, false).Scan(&ratio)
	if errors.Is(err, pgx.ErrNoRows) {
		return "0", nil
	}
	return ratio, err
}
