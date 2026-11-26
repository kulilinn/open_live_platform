CREATE TABLE gift_promotions (
    id BIGINT PRIMARY KEY CHECK (id > 0),
    gift_id BIGINT NOT NULL CHECK (gift_id > 0),
    name TEXT NOT NULL,
    description TEXT NOT NULL,
    starts_at TIMESTAMPTZ NOT NULL,
    ends_at TIMESTAMPTZ NOT NULL,
    deleted BOOLEAN NOT NULL DEFAULT FALSE,
    CHECK (ends_at > starts_at)
);

CREATE TABLE gift_purchase_limits (
    id BIGINT PRIMARY KEY CHECK (id > 0),
    gift_id BIGINT NOT NULL CHECK (gift_id > 0),
    promotion_id BIGINT NOT NULL CHECK (promotion_id >= 0),
    starts_at TIMESTAMPTZ NOT NULL,
    ends_at TIMESTAMPTZ NOT NULL,
    price BIGINT NOT NULL CHECK (price >= 0),
    income BIGINT CHECK (income >= 0),
    discount_type SMALLINT NOT NULL,
    discount_value INTEGER NOT NULL,
    per_user BIGINT NOT NULL CHECK (per_user >= 0),
    platform BIGINT NOT NULL CHECK (platform >= 0),
    listing SMALLINT NOT NULL,
    legacy_user_usage JSONB,
    legacy_platform_usage BIGINT CHECK (legacy_platform_usage >= 0),
    deleted BOOLEAN NOT NULL DEFAULT FALSE,
    CHECK (ends_at > starts_at)
);

ALTER TABLE gift_orders ADD COLUMN purchase_limit_id BIGINT REFERENCES gift_purchase_limits(id);
CREATE INDEX gift_orders_purchase_limit ON gift_orders(purchase_limit_id,sender_id,state)
    WHERE purchase_limit_id IS NOT NULL AND state IN (0,1);
