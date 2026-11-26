CREATE TABLE notices (
    id BIGINT PRIMARY KEY,
    notice_target SMALLINT NOT NULL,
    notice_type SMALLINT NOT NULL,
    receive_id BIGINT NOT NULL REFERENCES users(id),
    sender_avatar TEXT,
    content JSONB NOT NULL,
    biz_id BIGINT,
    status SMALLINT NOT NULL DEFAULT 1,
    create_time TIMESTAMPTZ NOT NULL
);
CREATE INDEX notices_by_user ON notices(receive_id,create_time DESC,id DESC);
