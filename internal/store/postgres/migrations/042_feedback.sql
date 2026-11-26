CREATE TABLE feedback (
	id BIGSERIAL PRIMARY KEY,
	user_id BIGINT NOT NULL REFERENCES users(id),
	feedback_type SMALLINT NOT NULL,
	content TEXT NOT NULL,
	attachment TEXT NOT NULL DEFAULT '',
	notification_status SMALLINT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX feedback_user_time ON feedback(user_id,created_at DESC);
