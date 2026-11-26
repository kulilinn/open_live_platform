CREATE TABLE legacy_business_history (
    source_table text NOT NULL,
    source_id text NOT NULL,
    owner_id bigint NOT NULL,
    record jsonb NOT NULL,
    PRIMARY KEY (source_table, source_id)
);
CREATE INDEX legacy_history_by_owner ON legacy_business_history (owner_id,source_table);
ALTER TABLE room_broadcast_metrics ADD COLUMN legacy_income bigint,
    ADD COLUMN legacy_new_fans bigint, ADD COLUMN legacy_gift_fans bigint;
