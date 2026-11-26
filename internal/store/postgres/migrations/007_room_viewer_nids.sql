-- Keep the sequence across broadcasts because old RTC tokens use the same channel.
ALTER TABLE rooms ADD COLUMN IF NOT EXISTS last_viewer_nid BIGINT NOT NULL DEFAULT 200000001
    CHECK (last_viewer_nid BETWEEN 200000001 AND 4294967295);

-- Mappings stay available after a viewer leaves and are removed when the broadcast ends.
CREATE TABLE IF NOT EXISTS room_viewer_nids (
    room_id BIGINT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    live_id BIGINT NOT NULL,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    nid BIGINT NOT NULL CHECK (nid BETWEEN 200000002 AND 4294967295),
    PRIMARY KEY (room_id, live_id, user_id),
    UNIQUE (room_id, live_id, nid)
);
