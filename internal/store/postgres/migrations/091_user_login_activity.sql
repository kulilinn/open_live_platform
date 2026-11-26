CREATE TABLE user_login_activity (
    user_id bigint PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    last_login_at timestamptz NOT NULL
);
CREATE INDEX user_login_activity_by_time ON user_login_activity(last_login_at);
INSERT INTO user_login_activity(user_id,last_login_at)
SELECT user_id,max(created_at) FROM sessions GROUP BY user_id;

CREATE FUNCTION track_user_login() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO user_login_activity(user_id,last_login_at) VALUES(NEW.user_id,statement_timestamp())
    ON CONFLICT(user_id) DO UPDATE SET last_login_at=greatest(user_login_activity.last_login_at,excluded.last_login_at);
    RETURN NULL;
END $$;

CREATE TRIGGER track_opaque_login AFTER INSERT ON sessions
FOR EACH ROW EXECUTE FUNCTION track_user_login();
CREATE TRIGGER track_jwt_login AFTER INSERT OR UPDATE OF family_id ON jwt_sessions
FOR EACH ROW WHEN (NEW.legacy_issuer IS NULL AND NEW.revoked_at IS NULL) EXECUTE FUNCTION track_user_login();
