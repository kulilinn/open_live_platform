CREATE TABLE public_catalog(
    section TEXT PRIMARY KEY,
    records JSONB NOT NULL CHECK(jsonb_typeof(records)='array')
);
CREATE INDEX users_search_name ON users(lower(coalesce(profile->>'SearchName',account)));
