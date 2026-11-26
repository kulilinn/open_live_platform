CREATE TABLE date_gift_blocks (
    channel BIGINT NOT NULL REFERENCES date_sessions(channel) ON DELETE CASCADE,
    gift_id BIGINT NOT NULL CHECK(gift_id>0),
    PRIMARY KEY(channel,gift_id)
);
