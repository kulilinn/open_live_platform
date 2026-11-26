CREATE TABLE google_identities (
    subject TEXT PRIMARY KEY,
    user_id BIGINT UNIQUE NOT NULL REFERENCES users(id) ON DELETE CASCADE
);
