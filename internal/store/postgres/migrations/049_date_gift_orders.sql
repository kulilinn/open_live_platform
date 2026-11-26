CREATE TABLE date_gift_orders (
    id BIGINT PRIMARY KEY,
    channel BIGINT NOT NULL REFERENCES date_sessions(channel) ON DELETE CASCADE,
    anchor_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    sender_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    request_key TEXT NOT NULL,
    provider_id BIGINT UNIQUE,
    state SMALLINT NOT NULL CHECK (state BETWEEN 0 AND 4),
    order_data JSONB NOT NULL,
    paid_at TIMESTAMPTZ,
    UNIQUE (sender_id, request_key)
);
CREATE UNIQUE INDEX date_gift_one_unaccepted_game ON date_gift_orders(channel) WHERE state=3;
CREATE INDEX date_gift_orders_channel_idx ON date_gift_orders(channel,id);
CREATE INDEX date_gift_orders_earnings_idx ON date_gift_orders(anchor_id,paid_at) WHERE state=1;

CREATE TABLE date_gift_queue (
    id BIGINT PRIMARY KEY,
    channel BIGINT NOT NULL REFERENCES date_sessions(channel) ON DELETE CASCADE,
    order_id BIGINT NOT NULL REFERENCES date_gift_orders(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    start_at TIMESTAMPTZ NOT NULL,
    end_at TIMESTAMPTZ NOT NULL,
    announced_at TIMESTAMPTZ,
    UNIQUE(channel,start_at)
);
CREATE INDEX date_gift_queue_end_idx ON date_gift_queue(channel,end_at);
CREATE INDEX date_gift_queue_start_due_idx ON date_gift_queue(start_at,end_at) WHERE announced_at IS NULL;
