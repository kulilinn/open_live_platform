CREATE SEQUENCE finance_payment_id_seq AS BIGINT START 5629499534213120;

CREATE TABLE finance_payment_attempts (
    id bigint PRIMARY KEY DEFAULT nextval('finance_payment_id_seq'),
    action text NOT NULL,
    request_key text NOT NULL,
    request_hash bytea NOT NULL CHECK(octet_length(request_hash)=32),
    request jsonb NOT NULL,
    user_id bigint NOT NULL CHECK(user_id>0),
    operator_id bigint NOT NULL CHECK(operator_id>0),
    operator_name text NOT NULL,
    point_delta bigint NOT NULL,
    reason text NOT NULL,
    state text NOT NULL DEFAULT 'reserved' CHECK(state IN ('reserved','confirmed','declined','unknown')),
    receipt jsonb,
    created_at timestamptz NOT NULL DEFAULT statement_timestamp(),
    finished_at timestamptz,
    UNIQUE(action,request_key),
    CHECK((finished_at IS NULL AND state='reserved') OR (finished_at IS NOT NULL AND state<>'reserved'))
);

CREATE INDEX finance_point_adjustments ON finance_payment_attempts(finished_at DESC,id DESC)
    WHERE state='confirmed' AND point_delta<>0;

CREATE TABLE finance_bad_debt_receipts (
    provider_order_id bigint PRIMARY KEY CHECK(provider_order_id>0),
    provider_order_no text NOT NULL UNIQUE CHECK(provider_order_no<>''),
    spender_id bigint NOT NULL CHECK(spender_id>0),
    charged_points bigint NOT NULL CHECK(charged_points>0),
    bad_debt_points bigint NOT NULL CHECK(bad_debt_points BETWEEN 0 AND charged_points),
    finance_attempt_id bigint NOT NULL REFERENCES finance_payment_attempts(id),
    observed_at timestamptz NOT NULL DEFAULT statement_timestamp()
);
