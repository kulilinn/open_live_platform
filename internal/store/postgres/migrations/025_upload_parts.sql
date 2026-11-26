CREATE TABLE uploads (
    object_key TEXT PRIMARY KEY,
    part_count INTEGER NOT NULL,
    complete BOOLEAN NOT NULL DEFAULT false,
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX uploads_by_expiry ON uploads(expires_at);
CREATE TABLE upload_parts (
    object_key TEXT NOT NULL REFERENCES uploads(object_key) ON DELETE CASCADE,
    part_index INTEGER NOT NULL,
    data BYTEA NOT NULL,
    PRIMARY KEY(object_key,part_index)
);
