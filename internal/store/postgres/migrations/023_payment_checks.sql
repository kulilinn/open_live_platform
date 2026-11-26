CREATE TABLE IF NOT EXISTS payment_checks (
    user_id BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    confirm_check BOOLEAN NOT NULL,
    adult_check BOOLEAN NOT NULL,
    confirm_pay_check BOOLEAN NOT NULL,
    confirm_country_check BOOLEAN NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS payment_large_order_attempts (
    user_id BIGINT NOT NULL REFERENCES users(id),
    request_key TEXT NOT NULL,
    provider_order TEXT UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    notified_at TIMESTAMPTZ,
    PRIMARY KEY (user_id, request_key),
    CHECK (notified_at IS NULL OR provider_order IS NOT NULL)
);
