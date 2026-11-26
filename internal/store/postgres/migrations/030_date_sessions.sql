CREATE TABLE date_hosts (
    anchor_id BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    channel BIGINT NOT NULL,
    state JSONB NOT NULL
);

CREATE UNIQUE INDEX date_hosts_channel_idx ON date_hosts(channel) WHERE channel<>0;

CREATE TABLE date_sessions (
    channel BIGINT PRIMARY KEY,
    anchor_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    audience_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    ended_at TIMESTAMPTZ,
    state JSONB NOT NULL,
    CHECK (anchor_id<>audience_id)
);

CREATE INDEX date_sessions_active_anchor_idx ON date_sessions(anchor_id) WHERE ended_at IS NULL;
CREATE INDEX date_sessions_active_audience_idx ON date_sessions(audience_id) WHERE ended_at IS NULL;

CREATE TABLE date_stt_tasks (
    channel BIGINT PRIMARY KEY REFERENCES date_sessions(channel) ON DELETE CASCADE,
    state JSONB NOT NULL
);
