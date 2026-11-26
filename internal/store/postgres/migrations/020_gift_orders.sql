CREATE SEQUENCE IF NOT EXISTS gift_order_id_seq AS BIGINT START WITH 17179869184 MAXVALUE 4611686018427387903;

CREATE TABLE IF NOT EXISTS gift_orders (
    id BIGINT PRIMARY KEY DEFAULT nextval('gift_order_id_seq'),
    room_id BIGINT NOT NULL REFERENCES rooms(id),
    live_id BIGINT NOT NULL,
    sender_id BIGINT NOT NULL REFERENCES users(id),
    receiver_id BIGINT NOT NULL REFERENCES users(id),
    request_key TEXT NOT NULL,
    payment_order TEXT NOT NULL UNIQUE,
    request JSONB NOT NULL,
    item JSONB NOT NULL,
    sender_info JSONB NOT NULL,
    receiver_info JSONB NOT NULL,
    sender_role SMALLINT NOT NULL,
    cost BIGINT NOT NULL CHECK (cost >= 0),
    income BIGINT NOT NULL CHECK (income >= 0 AND income <= cost),
    state SMALLINT NOT NULL DEFAULT 0 CHECK (state IN (0, 1, 2)),
    opened_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    paid_at TIMESTAMPTZ,
    provider_order_id BIGINT UNIQUE,
    receipt JSONB NOT NULL DEFAULT '{}',
    result JSONB NOT NULL DEFAULT '{}',
    UNIQUE (sender_id, request_key),
    CHECK (sender_id <> receiver_id),
    CHECK (state <> 1 OR (paid_at IS NOT NULL AND (cost = 0 OR provider_order_id IS NOT NULL)))
);

CREATE INDEX IF NOT EXISTS gift_live_income_idx ON gift_orders (room_id, live_id, paid_at DESC, id DESC) WHERE state=1;
CREATE INDEX IF NOT EXISTS gift_anchor_rank_idx ON gift_orders (receiver_id, paid_at DESC, sender_id) WHERE state=1 AND income>0;
