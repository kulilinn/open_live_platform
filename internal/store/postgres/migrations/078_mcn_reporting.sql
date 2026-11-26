CREATE INDEX mcn_history_by_agency_day ON legacy_business_history
    (owner_id,(record->>'create_time')) WHERE source_table='mcn_settle_statistic';

CREATE INDEX mcn_history_by_agency_live ON legacy_business_history
    (owner_id,(record->>'live_id')) WHERE source_table='mcn_settle_statistic';
