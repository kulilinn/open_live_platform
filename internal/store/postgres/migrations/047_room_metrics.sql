CREATE TABLE room_broadcast_metrics (
    live_id BIGINT PRIMARY KEY,
    room_id BIGINT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    started_at TIMESTAMPTZ NOT NULL,
    ended_at TIMESTAMPTZ,
    max_audience BIGINT NOT NULL DEFAULT 0,
    like_total BIGINT NOT NULL DEFAULT 0
);

INSERT INTO room_broadcast_metrics(live_id, room_id, started_at, ended_at, max_audience)
SELECT r.live_id, r.id, r.live_started_at, r.live_ended_at,
       (SELECT count(*) FROM room_members m WHERE m.room_id=r.id AND m.user_id<>r.owner_id)
FROM rooms r WHERE r.live_id>0 AND r.live_started_at IS NOT NULL;

CREATE TABLE room_broadcast_viewer_likes (
    live_id BIGINT NOT NULL REFERENCES room_broadcast_metrics(live_id) ON DELETE CASCADE,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    likes BIGINT NOT NULL,
    PRIMARY KEY(live_id, user_id)
);

CREATE TABLE room_reports (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    room TEXT NOT NULL,
    reporter_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    subject TEXT NOT NULL,
    description TEXT NOT NULL,
    reported_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp()
);
