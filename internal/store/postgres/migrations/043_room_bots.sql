CREATE TABLE room_bot_members (
    room_id BIGINT PRIMARY KEY,
    user_id BIGINT NOT NULL UNIQUE,
    live_id BIGINT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    FOREIGN KEY (room_id, user_id) REFERENCES room_members(room_id, user_id)
        ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED
);

CREATE INDEX room_bot_members_expiry_idx ON room_bot_members(expires_at);

CREATE TABLE room_bot_replies (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    room_id BIGINT NOT NULL REFERENCES room_bot_members(room_id) ON DELETE CASCADE,
    text TEXT NOT NULL,
    send_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX room_bot_replies_delivery_idx ON room_bot_replies(send_at, room_id);
CREATE INDEX room_bot_replies_room_idx ON room_bot_replies(room_id, send_at);
