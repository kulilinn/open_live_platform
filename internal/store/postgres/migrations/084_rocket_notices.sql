ALTER TABLE rocket_refuel_orders ADD COLUMN combo_at_settlement BIGINT NOT NULL DEFAULT 0;
CREATE INDEX room_rocket_contribution_experience ON room_rocket_contributions(user_id) WHERE platform_exp > 0;
