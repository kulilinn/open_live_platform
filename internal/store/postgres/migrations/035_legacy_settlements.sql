CREATE TABLE legacy_settlements (
    id BIGINT PRIMARY KEY CHECK (id > 0),
    anchor_id BIGINT NOT NULL CHECK (anchor_id > 0),
    giver_id BIGINT NOT NULL CHECK (giver_id >= 0),
    income_type SMALLINT NOT NULL CHECK (income_type BETWEEN -128 AND 127),
    points BIGINT NOT NULL,
    description TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX legacy_settlements_anchor_idx ON legacy_settlements(anchor_id, created_at DESC, id DESC);
CREATE INDEX gift_anchor_earnings_idx ON gift_orders(receiver_id, paid_at DESC, id DESC) WHERE state=1;
