CREATE TABLE mcn_finance_stat_snapshots (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source_table text NOT NULL CHECK(source_table IN ('mcn_settle_statistic','mcn_settle_agency_streamer_statistic','mcn_settle_platform_statistic')),
    agency_id bigint NOT NULL CHECK(agency_id>0),
    entry_key text NOT NULL,
    report_date date NOT NULL,
    record jsonb NOT NULL,
    refreshed_at timestamptz NOT NULL DEFAULT statement_timestamp(),
    UNIQUE(source_table,agency_id,entry_key)
);

CREATE INDEX mcn_stats_by_date ON mcn_finance_stat_snapshots(source_table,agency_id,report_date);

CREATE VIEW mcn_finance_records AS
SELECT h.source_table,h.source_id,h.owner_id,h.record
FROM legacy_business_history h
WHERE h.source_table IN ('mcn_settle_statistic','mcn_settle_agency_streamer_statistic','mcn_settle_platform_statistic')
  AND NOT EXISTS (
    SELECT 1 FROM mcn_finance_stat_snapshots n WHERE n.source_table=h.source_table AND n.agency_id=h.owner_id
      AND CASE h.source_table
        WHEN 'mcn_settle_statistic' THEN false
        WHEN 'mcn_settle_agency_streamer_statistic' THEN
           n.record->>'streamer_id'=h.record->>'streamer_id' AND left(n.record->>'create_time',7)=left(h.record->>'create_time',7)
        ELSE n.record->>'create_time'=h.record->>'create_time' END
  )
UNION ALL
SELECT n.source_table,(-n.id)::text,n.agency_id,n.record FROM mcn_finance_stat_snapshots n
WHERE n.source_table<>'mcn_settle_statistic' OR NOT EXISTS(
    SELECT 1 FROM legacy_business_history h WHERE h.source_table='mcn_settle_statistic' AND h.owner_id=n.agency_id
    AND h.record->>'live_id'=n.entry_key
);
