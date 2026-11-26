CREATE TABLE room_gift_effect_settings (
    room_id BIGINT PRIMARY KEY REFERENCES rooms(id) ON DELETE CASCADE,
    reduce_time_ratio DOUBLE PRECISION NOT NULL CHECK (reduce_time_ratio BETWEEN 0 AND 100)
);

CREATE TABLE room_gift_effects (
    order_id BIGINT PRIMARY KEY REFERENCES gift_orders(id) ON DELETE CASCADE,
    room_id BIGINT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    live_id BIGINT NOT NULL REFERENCES room_broadcast_metrics(live_id) ON DELETE CASCADE,
    effect SMALLINT NOT NULL CHECK (effect IN (2,3,4,5,6)),
    start_at TIMESTAMPTZ,
    end_at TIMESTAMPTZ,
    ended_at TIMESTAMPTZ,
    announced_at TIMESTAMPTZ,
    speed INTEGER NOT NULL DEFAULT 0,
    final_result JSONB
);

CREATE INDEX room_gift_effects_active_idx ON room_gift_effects(room_id,live_id,effect,start_at,end_at)
    WHERE ended_at IS NULL;
CREATE INDEX room_gift_effects_start_due_idx ON room_gift_effects(start_at,end_at,room_id)
    WHERE ended_at IS NULL AND announced_at IS NULL AND effect BETWEEN 2 AND 5;

CREATE TABLE room_rocket_contributions (
    order_id BIGINT NOT NULL REFERENCES room_gift_effects(order_id) ON DELETE CASCADE,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    score BIGINT NOT NULL CHECK(score >= 0),
    PRIMARY KEY(order_id,user_id)
);
