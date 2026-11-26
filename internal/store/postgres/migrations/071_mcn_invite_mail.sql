CREATE TABLE mcn_invite_mail (
    id BIGINT PRIMARY KEY REFERENCES notices(id) ON DELETE CASCADE,
    membership_id BIGINT NOT NULL REFERENCES backoffice_mcn_streamers(id) ON DELETE CASCADE,
    invite_hash BYTEA NOT NULL,
    subject TEXT NOT NULL,
    body TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts>=0),
    next_attempt TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    lease_id TEXT,
    lease_until TIMESTAMPTZ,
    sent_at TIMESTAMPTZ
);
CREATE INDEX mcn_invite_mail_work_idx ON mcn_invite_mail(next_attempt,id) WHERE sent_at IS NULL;
