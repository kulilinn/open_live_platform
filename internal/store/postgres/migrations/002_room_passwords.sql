ALTER TABLE rooms ADD COLUMN IF NOT EXISTS password_hash BYTEA;

CREATE TABLE IF NOT EXISTS room_password_attempts (
    room_id BIGINT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    failed_attempts INTEGER NOT NULL CHECK (failed_attempts > 0),
    PRIMARY KEY (room_id, user_id)
);
