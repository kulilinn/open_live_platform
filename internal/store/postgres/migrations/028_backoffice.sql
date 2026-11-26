CREATE TABLE IF NOT EXISTS backoffice_sessions (
    area TEXT NOT NULL CHECK (area IN ('admin','mcn')),
    principal_id BIGINT NOT NULL,
    fingerprint BYTEA NOT NULL,
    access_hash BYTEA NOT NULL UNIQUE,
    refresh_hash BYTEA NOT NULL UNIQUE,
    access_expires TIMESTAMPTZ NOT NULL,
    refresh_expires TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (area, principal_id)
);

CREATE SEQUENCE IF NOT EXISTS backoffice_log_id_seq AS BIGINT START WITH 1099511627776;

CREATE TABLE IF NOT EXISTS backoffice_logs (
    id BIGINT PRIMARY KEY DEFAULT nextval('backoffice_log_id_seq'),
    area TEXT NOT NULL,
    tenant TEXT NOT NULL,
    operator_id BIGINT NOT NULL,
    operator_name TEXT NOT NULL,
    path TEXT NOT NULL,
    target TEXT NOT NULL DEFAULT '',
    action TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT '',
    create_time TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp()
);
CREATE INDEX IF NOT EXISTS backoffice_log_tenant_idx ON backoffice_logs(area,tenant,create_time DESC,id DESC);

ALTER TABLE rooms ADD COLUMN IF NOT EXISTS moderation_until BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS moderation_total BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS moderation_reason TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS backoffice_mcn_streamers (
    id BIGINT PRIMARY KEY,
    mcn_id BIGINT NOT NULL CHECK (mcn_id > 0),
    streamer_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    manager_id BIGINT,
    manager_name TEXT,
    email TEXT NOT NULL,
    name TEXT NOT NULL,
    ratio NUMERIC NOT NULL,
    manager_ratio NUMERIC NOT NULL,
    agency_ratio NUMERIC NOT NULL,
    fan_num INTEGER,
    status SMALLINT NOT NULL,
    create_time TIMESTAMPTZ NOT NULL,
    UNIQUE(mcn_id, streamer_id)
);
CREATE INDEX IF NOT EXISTS backoffice_mcn_streamers_scope_idx ON backoffice_mcn_streamers(mcn_id,manager_id,create_time DESC,id DESC);
