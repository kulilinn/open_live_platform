CREATE TABLE anchor_profiles (
    user_id BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    state SMALLINT NOT NULL DEFAULT 0 CHECK(state IN (0, 1, 2)),
    reason TEXT NOT NULL DEFAULT '',
    channel TEXT NOT NULL DEFAULT '',
    kyc_status SMALLINT NOT NULL DEFAULT 0,
    task_started_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    details JSONB NOT NULL DEFAULT '{}'
);
CREATE TABLE anchor_kyc_applicants (
    user_id BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    applicant_id TEXT NOT NULL UNIQUE CHECK(applicant_id<>'')
);
