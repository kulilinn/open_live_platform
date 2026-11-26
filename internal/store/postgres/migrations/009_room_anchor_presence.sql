ALTER TABLE rooms ADD COLUMN IF NOT EXISTS anchor_away BOOLEAN NOT NULL DEFAULT FALSE;

CREATE TABLE IF NOT EXISTS room_anchor_connections (
    room_id BIGINT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    live_id BIGINT NOT NULL,
    connection_id TEXT NOT NULL,
    PRIMARY KEY (room_id, live_id, connection_id)
);
