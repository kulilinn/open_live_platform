CREATE TABLE anchor_channel_users (
    user_id BIGINT PRIMARY KEY,
    channel TEXT NOT NULL CHECK(channel<>''),
    joined_at TIMESTAMPTZ,
    snapshot TEXT NOT NULL
);
CREATE INDEX anchor_channel_users_lookup ON anchor_channel_users(channel, joined_at);

CREATE TABLE anchor_channel_spends (
    id TEXT NOT NULL,
    user_id BIGINT NOT NULL,
    points BIGINT NOT NULL CHECK(points>0),
    spent_at TIMESTAMPTZ NOT NULL,
    snapshot TEXT NOT NULL,
    PRIMARY KEY(id,user_id)
);
CREATE INDEX anchor_channel_spends_lookup ON anchor_channel_spends(user_id,spent_at);

CREATE TABLE anchor_channel_rates (
    channel TEXT PRIMARY KEY,
    revenue_ratio INTEGER NOT NULL CHECK(revenue_ratio>=0),
    snapshot TEXT NOT NULL
);

CREATE TABLE anchor_channel_snapshot (
    active BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK(active),
    completed_at TIMESTAMPTZ NOT NULL
);
