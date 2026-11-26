ALTER TABLE anchor_posts ADD COLUMN livemedia_imported BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE anchor_post_comments ADD COLUMN livemedia_imported BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE public_shares ADD COLUMN livemedia_imported BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE subscription_groups ADD COLUMN livemedia_imported BOOLEAN NOT NULL DEFAULT FALSE;

CREATE TABLE legacy_anchor_post_unlocks (
    post_id BIGINT NOT NULL REFERENCES anchor_posts(id) ON DELETE CASCADE,
    actor_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    PRIMARY KEY (post_id,actor_id)
);

CREATE TABLE legacy_anchor_post_tips (
    post_id BIGINT NOT NULL REFERENCES anchor_posts(id) ON DELETE CASCADE,
    actor_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    points BIGINT NOT NULL CHECK (points>0),
    tipped_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (post_id,actor_id)
);

CREATE TABLE legacy_subscription_lists_imported (
    owner_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind SMALLINT NOT NULL CHECK (kind IN (2,3)),
    PRIMARY KEY (owner_id,kind)
);
