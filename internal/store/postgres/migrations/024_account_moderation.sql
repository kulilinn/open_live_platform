ALTER TABLE users ALTER COLUMN email DROP NOT NULL,
    ADD COLUMN account_status SMALLINT NOT NULL DEFAULT 0 CHECK (account_status IN (0,1,2)),
    ADD COLUMN account_limit_until BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN account_violation_id BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN legacy_profile_email TEXT;

CREATE TABLE IF NOT EXISTS user_violation_records (
    id BIGINT PRIMARY KEY,
    user_id BIGINT NOT NULL,
    reason TEXT NOT NULL,
    limit_duration BIGINT NOT NULL,
    create_time TIMESTAMPTZ NOT NULL,
    create_by_name TEXT NOT NULL,
    ty SMALLINT NOT NULL
);

CREATE INDEX IF NOT EXISTS user_violation_history_idx ON user_violation_records (user_id, create_time DESC);
