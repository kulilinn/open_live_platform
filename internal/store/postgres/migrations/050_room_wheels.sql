CREATE TABLE room_wheel_configs (
    anchor_id BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    price BIGINT NOT NULL DEFAULT 0,
    options TEXT[] NOT NULL DEFAULT '{}',
    open_status SMALLINT NOT NULL DEFAULT 1 CHECK (open_status IN (0,1))
);

CREATE TABLE room_wheel_orders (
    id BIGINT PRIMARY KEY,
    room_id BIGINT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    live_id BIGINT NOT NULL REFERENCES room_broadcast_metrics(live_id) ON DELETE CASCADE,
    anchor_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    request_key TEXT NOT NULL,
    state SMALLINT NOT NULL CHECK (state BETWEEN 0 AND 2),
    order_data JSONB NOT NULL,
    provider_id BIGINT UNIQUE,
    paid_at TIMESTAMPTZ,
    UNIQUE (user_id,request_key)
);

CREATE INDEX room_wheel_order_history_idx ON room_wheel_orders(room_id,live_id,paid_at,id) WHERE state=1;
CREATE INDEX room_wheel_order_earnings_idx ON room_wheel_orders(anchor_id,paid_at) WHERE state=1;

CREATE TABLE room_wheel_recommendations (
    title TEXT NOT NULL,
    suggestion TEXT NOT NULL,
    PRIMARY KEY(title,suggestion)
);
