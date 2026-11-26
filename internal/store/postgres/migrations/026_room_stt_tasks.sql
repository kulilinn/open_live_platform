CREATE TABLE room_stt_tasks (
    room_id BIGINT PRIMARY KEY REFERENCES rooms(id) ON DELETE CASCADE,
    live_id BIGINT NOT NULL,
    api_version TEXT NOT NULL CHECK (api_version IN ('v1', 'v2')),
    remote_id TEXT NOT NULL,
    builder_token TEXT NOT NULL DEFAULT '',
    sequence BIGINT NOT NULL DEFAULT 0
);
