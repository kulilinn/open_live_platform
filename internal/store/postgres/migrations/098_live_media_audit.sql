CREATE TABLE live_media_audit_tasks (
    live_id BIGINT PRIMARY KEY REFERENCES room_broadcast_metrics(live_id) ON DELETE CASCADE,
    room_id BIGINT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    anchor_id BIGINT NOT NULL REFERENCES users(id),
    anchor_name TEXT NOT NULL,
    request_id TEXT,
    opening BOOLEAN NOT NULL DEFAULT FALSE,
    retry BOOLEAN NOT NULL DEFAULT FALSE,
    version BIGINT NOT NULL DEFAULT 0,
    failures INTEGER NOT NULL DEFAULT 0,
    available_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp()
);
CREATE INDEX live_media_audit_available ON live_media_audit_tasks(available_at);

CREATE TABLE live_media_audit_requests (
    request_id TEXT PRIMARY KEY,
    live_id BIGINT NOT NULL REFERENCES live_media_audit_tasks(live_id) ON DELETE CASCADE,
    started_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    stopped_at TIMESTAMPTZ
);

CREATE TABLE live_media_audit_history (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    request_id TEXT NOT NULL REFERENCES live_media_audit_requests(request_id) ON DELETE CASCADE,
    callback_key TEXT NOT NULL,
    kind TEXT NOT NULL,
    payload JSONB NOT NULL,
    applied BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    UNIQUE(request_id,callback_key)
);
