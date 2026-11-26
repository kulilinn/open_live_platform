ALTER TABLE subscription_plans ADD COLUMN IF NOT EXISTS livemedia_imported BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS legacy_promotion TEXT NOT NULL DEFAULT '';

DROP INDEX IF EXISTS subscription_single_free_plan;
CREATE UNIQUE INDEX IF NOT EXISTS subscription_single_native_free_plan ON subscription_plans(anchor_id)
    WHERE plan_type=2 AND NOT livemedia_imported;

ALTER TABLE subscription_members ADD COLUMN IF NOT EXISTS legacy_id TEXT,
    ADD COLUMN IF NOT EXISTS legacy_level SMALLINT,
    ADD COLUMN IF NOT EXISTS legacy_first SMALLINT,
    ADD COLUMN IF NOT EXISTS bundle TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS renewal_authorized BOOLEAN NOT NULL DEFAULT TRUE;

CREATE TABLE IF NOT EXISTS legacy_subscription_settlements (
    id BIGINT PRIMARY KEY,
    no TEXT NOT NULL,
    fan_id BIGINT NOT NULL,
    anchor_id BIGINT NOT NULL,
    settle_type SMALLINT NOT NULL,
    settle_ld BIGINT NOT NULL,
    expend_point BIGINT NOT NULL,
    point_transaction_id BIGINT NOT NULL,
    point_transaction_no TEXT NOT NULL,
    subject TEXT NOT NULL,
    bad_debt_point BIGINT NOT NULL,
    bad_debt_ld BIGINT NOT NULL,
    status SMALLINT NOT NULL,
    settle_time TIMESTAMPTZ,
    actual_settle_ld BIGINT NOT NULL,
    settle_config_id BIGINT,
    settle_ratio NUMERIC,
    create_time TIMESTAMPTZ NOT NULL,
    update_time TIMESTAMPTZ,
    verified_plan_id BIGINT REFERENCES subscription_plans(id) ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS legacy_subscription_settlements_participants ON legacy_subscription_settlements (fan_id,anchor_id,create_time DESC);
