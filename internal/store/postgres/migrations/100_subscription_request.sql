ALTER TABLE subscription_orders ADD COLUMN request_bundle BYTEA NOT NULL DEFAULT '',
    ADD COLUMN request_key TEXT;
CREATE UNIQUE INDEX subscription_purchase_request_key ON subscription_orders(fan_id,request_key) WHERE request_key IS NOT NULL;
