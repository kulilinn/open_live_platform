ALTER TABLE subscription_plans ADD COLUMN IF NOT EXISTS bundles TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS promotion TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS promotion_ends_at TIMESTAMPTZ;

ALTER TABLE subscription_members ADD COLUMN IF NOT EXISTS promotion_redeemed BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE subscription_orders ADD COLUMN IF NOT EXISTS renewal_from TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS subscription_last_paid_order ON subscription_orders (fan_id,plan_id,confirmed_at DESC) WHERE status=1;
