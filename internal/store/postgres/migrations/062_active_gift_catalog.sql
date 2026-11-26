CREATE TABLE backoffice_gifts (
    id BIGINT PRIMARY KEY CHECK(id>0),
    item JSONB NOT NULL,
    deleted BOOLEAN NOT NULL DEFAULT FALSE,
    changed_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp()
);
