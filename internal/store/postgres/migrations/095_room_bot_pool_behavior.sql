CREATE TABLE room_bot_pool (
    user_id BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    imported_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp()
);

ALTER TABLE room_bot_members ADD COLUMN like_at TIMESTAMPTZ,
    ADD COLUMN daily_at TIMESTAMPTZ;

CREATE INDEX room_bot_members_like_idx ON room_bot_members(like_at,room_id) WHERE like_at IS NOT NULL;
CREATE INDEX room_bot_members_daily_idx ON room_bot_members(daily_at,room_id) WHERE daily_at IS NOT NULL;
