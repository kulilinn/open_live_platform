ALTER TABLE nft_card_inventory
    DROP CONSTRAINT nft_card_inventory_status_check,
    ADD CONSTRAINT nft_card_inventory_trade_status_check CHECK (status BETWEEN 1 AND 5),
    ADD COLUMN order_account TEXT,
    ADD COLUMN pre_order_account TEXT,
    ADD COLUMN pre_order_name TEXT,
    ADD COLUMN description TEXT,
    ADD COLUMN created_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    ADD COLUMN updated_at TIMESTAMPTZ,
    ADD COLUMN sale_at TIMESTAMPTZ;

CREATE TABLE nft_backoffice_requests (
    actor_id BIGINT NOT NULL,
    request_key TEXT NOT NULL CHECK (length(request_key) BETWEEN 1 AND 128),
    fingerprint BYTEA NOT NULL CHECK (octet_length(fingerprint)=32),
    created_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    PRIMARY KEY(actor_id,request_key)
);
