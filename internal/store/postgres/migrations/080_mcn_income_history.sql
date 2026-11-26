CREATE INDEX mcn_finance_history_by_agency_period ON legacy_business_history
    (source_table,owner_id,(record->>'create_time'))
    WHERE source_table IN ('mcn_settle_agency_streamer_statistic','mcn_settle_platform_statistic');
