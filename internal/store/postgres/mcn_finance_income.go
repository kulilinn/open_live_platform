package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/admin"
)

func (s *Store) MCNFinanceAgencyIncome(ctx context.Context, q admin.MCNFinanceIncomeQuery) ([]admin.MCNFinanceAgencyIncome, error) {
	rows, err := s.pool.Query(ctx, `SELECT h.owner_id,COALESCE(a.name,''),COALESCE(a.super_name,''),
		sum((h.record->>'ld')::bigint)::bigint,(h.record->>'ratio')::numeric::text FROM mcn_finance_records h
		LEFT JOIN mcn_agency_directory a ON a.id=h.owner_id
		WHERE h.source_table='mcn_settle_platform_statistic' AND h.record->>'create_time'=$1
		AND ($2::bigint IS NULL OR h.owner_id=$2) AND ($3::text IS NULL OR strpos(lower(a.name),lower($3))>0)
		GROUP BY h.owner_id,a.name,a.super_name,(h.record->>'ratio')::numeric ORDER BY h.owner_id,5`, q.Month, q.AgencyID, q.AgencyName)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (admin.MCNFinanceAgencyIncome, error) {
		var item admin.MCNFinanceAgencyIncome
		err := row.Scan(&item.AgencyID, &item.Name, &item.SuperName, &item.LD, &item.Ratio)
		return item, err
	})
}

func (s *Store) MCNFinanceStreamerIncome(ctx context.Context, q admin.MCNFinanceIncomeQuery) ([]admin.MCNFinanceStreamerIncome, int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT h.owner_id,a.name,m.name,j.ld,COALESCE(j.ratio,0)::text,
		j.live_start_time,j.live_end_time,j.create_time,sum(j.ld) OVER ()::bigint
		FROM mcn_finance_records h CROSS JOIN LATERAL jsonb_to_record(h.record) j
		(streamer_id bigint,ld bigint,ratio numeric,live_start_time bigint,live_end_time bigint,create_time text)
		LEFT JOIN mcn_agency_directory a ON a.id=h.owner_id
		LEFT JOIN backoffice_mcn_streamers m ON m.mcn_id=h.owner_id AND m.streamer_id=j.streamer_id AND NOT m.superseded
		WHERE h.source_table='mcn_settle_statistic' AND j.create_time BETWEEN $1 AND $2
		AND ($3::bigint IS NULL OR h.owner_id=$3) AND ($4::text IS NULL OR strpos(lower(a.name),lower($4))>0)
		AND ($5::text IS NULL OR lower(m.name)=lower($5)) ORDER BY j.create_time DESC,h.owner_id,h.source_id::bigint`,
		q.From, q.To, q.AgencyID, q.AgencyName, q.StreamerName)
	if err != nil {
		return nil, 0, err
	}
	var total int64
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (admin.MCNFinanceStreamerIncome, error) {
		var item admin.MCNFinanceStreamerIncome
		err := row.Scan(&item.AgencyID, &item.AgencyName, &item.StreamerName, &item.LD, &item.Ratio,
			&item.LiveStart, &item.LiveEnd, &item.Date, &total)
		return item, err
	})
	return items, total, err
}

func (s *Store) RefreshMCNIncome(ctx context.Context, actor admin.Principal, path, kind string, q admin.MCNDateQuery) error {
	if q.Start == nil || q.End == nil || q.AgencyID <= 0 || (actor.Area == admin.MCNArea && (!actor.Admin || actor.AgencyID != q.AgencyID)) {
		return admin.ErrBackofficeForbidden
	}
	return s.mcnChange(ctx, func(tx pgx.Tx) error {
		switch kind {
		case "daily":
			if err := refreshMCNDaily(ctx, tx, q); err != nil {
				return err
			}
		case "streamer", "platform":
			if err := refreshMCNMonthly(ctx, tx, kind, q); err != nil {
				return err
			}
		default:
			return admin.ErrSystemInvalid
		}
		return auditMCNStreamer(ctx, tx, actor, path, "refresh_"+kind, q.AgencyID)
	})
}

func refreshMCNDaily(ctx context.Context, tx pgx.Tx, q admin.MCNDateQuery) error {
	var count int64
	err := tx.QueryRow(ctx, mcnReportEntries+`, saved AS (
		INSERT INTO mcn_finance_stat_snapshots AS target(source_table,agency_id,entry_key,report_date,record)
		SELECT 'mcn_settle_statistic',$1,r.live_key,r.day,
			jsonb_build_object('mcn_id',$1::bigint,'live_id',r.live_key::bigint,'streamer_id',r.streamer_id,'manager_id',r.manager_id,
			'ld',r.ld,'viewer_num',r.viewers,'new_fans_num',r.fans,'ratio',m.ratio,'live_start_time',floor(extract(epoch FROM r.started))::bigint,
			'live_end_time',floor(extract(epoch FROM r.ended))::bigint,'create_time',to_char(r.day,'YYYY-MM-DD'))
		FROM records r JOIN backoffice_mcn_streamers m ON m.mcn_id=$1 AND m.streamer_id=r.streamer_id WHERE NOT r.archived
		ON CONFLICT(source_table,agency_id,entry_key) DO UPDATE SET report_date=excluded.report_date,record=excluded.record,
			refreshed_at=statement_timestamp() WHERE target.record IS DISTINCT FROM excluded.record RETURNING true
	) SELECT count(*) FROM records`,
		q.AgencyID, q.ManagerID, q.Streamers, q.Start, q.End, true).Scan(&count)
	if err == nil && count == 0 {
		return admin.ErrBackofficeNotFound
	}
	return err
}

const mcnMonthlyRecords = `WITH daily AS (
	SELECT j.streamer_id,j.ld,date_trunc('month',CASE WHEN length(j.create_time)=7 THEN (j.create_time||'-01')::date ELSE j.create_time::date END)::date AS month
	FROM mcn_finance_records h CROSS JOIN LATERAL jsonb_to_record(h.record) j(streamer_id bigint,ld bigint,create_time text)
	WHERE h.source_table='mcn_settle_statistic' AND h.owner_id=$1 AND left(j.create_time,7) BETWEEN $2 AND $3
)`

func refreshMCNMonthly(ctx context.Context, tx pgx.Tx, kind string, q admin.MCNDateQuery) error {
	start, end := q.Start.Format("2006-01"), q.End.Format("2006-01")
	var records, validRates int64
	var err error
	if kind == "platform" {
		err = tx.QueryRow(ctx, mcnMonthlyRecords+`, totals AS (
			SELECT month,sum(ld)::bigint AS ld FROM daily GROUP BY month
		), rated AS (
			SELECT t.month,t.ld,sharing.ratio FROM totals t LEFT JOIN LATERAL (
				SELECT ratio FROM mcn_template_directory WHERE agency_id=0 AND template_id=1 AND NOT disabled
				AND t.ld BETWEEN lower_bound AND upper_bound ORDER BY lower_bound DESC,id LIMIT 1
			) sharing ON TRUE
		), saved AS (
			INSERT INTO mcn_finance_stat_snapshots AS target(source_table,agency_id,entry_key,report_date,record)
			SELECT 'mcn_settle_platform_statistic',$1,to_char(month,'YYYY-MM'),month,
				jsonb_build_object('mcn_id',$1::bigint,'ld',ld,'ratio',ratio,'create_time',to_char(month,'YYYY-MM'))
			FROM rated WHERE ratio IS NOT NULL
			ON CONFLICT(source_table,agency_id,entry_key) DO UPDATE SET record=excluded.record,refreshed_at=statement_timestamp()
			WHERE target.record IS DISTINCT FROM excluded.record RETURNING true
		) SELECT count(*),count(ratio) FROM rated`, q.AgencyID, start, end).Scan(&records, &validRates)
	} else {
		err = tx.QueryRow(ctx, mcnMonthlyRecords+`, totals AS (
			SELECT d.streamer_id,d.month,sum(d.ld)::bigint AS ld FROM daily d GROUP BY d.streamer_id,d.month
		), rated AS (
			SELECT t.*,m.manager_id,m.manager_ratio,m.agency_ratio FROM totals t JOIN backoffice_mcn_streamers m
			ON m.mcn_id=$1 AND m.streamer_id=t.streamer_id AND NOT m.superseded
		), saved AS (
			INSERT INTO mcn_finance_stat_snapshots AS target(source_table,agency_id,entry_key,report_date,record)
			SELECT 'mcn_settle_agency_streamer_statistic',$1,streamer_id::text||':'||to_char(month,'YYYY-MM'),month,
				jsonb_build_object('mcn_id',$1::bigint,'streamer_id',streamer_id,'manager_id',manager_id,'ld',ld,
				'ratio',agency_ratio,'manager_ratio',manager_ratio,'agency_ratio',agency_ratio,'create_time',to_char(month,'YYYY-MM-DD'))
			FROM rated
			ON CONFLICT(source_table,agency_id,entry_key) DO UPDATE SET record=excluded.record,refreshed_at=statement_timestamp()
			WHERE target.record IS DISTINCT FROM excluded.record RETURNING true
		) SELECT (SELECT count(*) FROM totals),count(*) FROM rated`, q.AgencyID, start, end).Scan(&records, &validRates)
	}
	if err == nil && records == 0 {
		return admin.ErrBackofficeNotFound
	}
	if err == nil && validRates != records {
		return admin.ErrMCNReconcile
	}
	return err
}
