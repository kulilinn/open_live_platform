CREATE TABLE post_media_audit_history (
    post_id BIGINT NOT NULL REFERENCES anchor_posts(id) ON DELETE CASCADE,
    request_id TEXT NOT NULL,
    manual BOOLEAN NOT NULL,
    status INTEGER NOT NULL CHECK (status IN (1,2,3)),
    created_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    PRIMARY KEY(post_id,request_id,manual,status)
);
