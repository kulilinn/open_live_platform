CREATE TABLE gift_boxes (
    id BIGINT PRIMARY KEY CHECK (id > 0),
    name TEXT NOT NULL,
    limit_ids TEXT NOT NULL,
    deleted BOOLEAN NOT NULL DEFAULT FALSE
);

CREATE TABLE gift_shortcuts (
    id BIGINT PRIMARY KEY CHECK (id > 0),
    limit_id BIGINT NOT NULL CHECK (limit_id >= 0),
    promotion_id BIGINT NOT NULL DEFAULT 0 CHECK (promotion_id >= 0),
    sort INTEGER NOT NULL DEFAULT 0,
    deleted BOOLEAN NOT NULL DEFAULT FALSE
);

CREATE TABLE gift_collection_bootstrap (
    kind TEXT PRIMARY KEY
);
