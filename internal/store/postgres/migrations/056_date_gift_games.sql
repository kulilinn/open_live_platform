CREATE TABLE date_gift_games (
    channel BIGINT PRIMARY KEY REFERENCES date_sessions(channel) ON DELETE CASCADE,
    game_data JSONB NOT NULL
);

CREATE TABLE date_gift_game_orders (
    order_id BIGINT PRIMARY KEY REFERENCES date_gift_orders(id) ON DELETE CASCADE,
    game_id BIGINT NOT NULL UNIQUE
);
