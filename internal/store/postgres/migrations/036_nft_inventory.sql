CREATE TABLE nft_card_inventory (
    id BIGINT PRIMARY KEY CHECK (id BETWEEN 0 AND 4294967295),
    name TEXT NOT NULL,
    english TEXT NOT NULL,
    chinese TEXT NOT NULL,
    picture TEXT NOT NULL,
    address TEXT NOT NULL CHECK (address='' OR address ~ '^0x[0-9a-f]{40}$'),
    status SMALLINT NOT NULL CHECK (status IN (1, 5)),
    sold_at TIMESTAMPTZ
);
