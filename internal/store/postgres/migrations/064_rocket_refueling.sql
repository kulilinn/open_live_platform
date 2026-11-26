CREATE TABLE rocket_jackpot (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    balance BIGINT NOT NULL CHECK (balance >= 0),
    reserved BIGINT NOT NULL DEFAULT 0 CHECK (reserved >= 0 AND reserved <= balance)
);

CREATE TABLE rocket_refuel_orders (
    order_id BIGINT PRIMARY KEY REFERENCES gift_orders(id) ON DELETE CASCADE,
    launch_id BIGINT NOT NULL REFERENCES room_gift_effects(order_id) ON DELETE CASCADE,
    reserved BIGINT NOT NULL CHECK (reserved >= 0),
    plan JSONB NOT NULL,
    added_millis BIGINT NOT NULL DEFAULT 0,
    end_at_after TIMESTAMPTZ
);
CREATE INDEX rocket_refuel_launch_idx ON rocket_refuel_orders(launch_id);

CREATE TABLE rocket_pool_ledger (
    order_id BIGINT PRIMARY KEY REFERENCES gift_orders(id) ON DELETE CASCADE,
    contribution BIGINT NOT NULL CHECK (contribution >= 0),
    platform_points BIGINT NOT NULL CHECK (platform_points >= 0),
    drawn BIGINT NOT NULL CHECK (drawn >= 0),
    balance BIGINT NOT NULL CHECK (balance >= 0)
);

CREATE TABLE rocket_launch_amounts (
    order_id BIGINT PRIMARY KEY REFERENCES gift_orders(id) ON DELETE CASCADE,
    contribution BIGINT NOT NULL,
    platform_points BIGINT NOT NULL
);

CREATE TABLE rocket_refuel_awards (
    id BIGINT GENERATED ALWAYS AS IDENTITY UNIQUE,
    order_id BIGINT NOT NULL REFERENCES rocket_refuel_orders(order_id) ON DELETE CASCADE,
    draw INTEGER NOT NULL,
    anchor_id BIGINT NOT NULL REFERENCES users(id),
    gift_id BIGINT NOT NULL,
    gift_name TEXT NOT NULL,
    gift_icon TEXT NOT NULL,
    income BIGINT NOT NULL CHECK (income >= 0),
    settled_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(order_id,draw)
);
CREATE INDEX rocket_refuel_awards_earnings ON rocket_refuel_awards(anchor_id,settled_at);

ALTER TABLE room_gift_effects ADD COLUMN fuel_remainder BIGINT NOT NULL DEFAULT 0 CHECK (fuel_remainder BETWEEN 0 AND 999);
ALTER TABLE room_rocket_contributions ADD COLUMN combo BIGINT NOT NULL DEFAULT 0;
ALTER TABLE room_rocket_contributions ADD COLUMN multiplier BIGINT NOT NULL DEFAULT 0;
ALTER TABLE room_rocket_contributions ADD COLUMN platform_exp BIGINT NOT NULL DEFAULT 0;

CREATE VIEW room_gift_income AS
SELECT o.id,o.room_id,o.live_id,o.sender_id,o.receiver_id,o.paid_at,
       (o.request->>'gift_id')::bigint AS gift_id,o.item->>'name' AS gift_name,o.item->>'icon' AS gift_icon,
       o.income,o.sender_info->>'name' AS sender_name,2::smallint AS income_type
FROM gift_orders o WHERE o.state=1 AND COALESCE(o.item->>'effectControlType','0')<>'7'
UNION ALL
SELECT a.id,o.room_id,o.live_id,o.sender_id,o.receiver_id,a.settled_at,a.gift_id,a.gift_name,a.gift_icon,
       a.income,o.sender_info->>'name',2::smallint
FROM rocket_refuel_awards a JOIN gift_orders o ON o.id=a.order_id WHERE o.state=1
UNION ALL
SELECT w.id,w.room_id,w.live_id,w.user_id,w.anchor_id,w.paid_at,w.anchor_id,'转盘','',
       (w.order_data->>'Price')::bigint,w.order_data#>>'{Result,user_name}',8::smallint
FROM room_wheel_orders w WHERE w.state=1;
