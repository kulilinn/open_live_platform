CREATE TABLE anchor_post_orders (
    order_no TEXT PRIMARY KEY,
    post_id BIGINT NOT NULL REFERENCES anchor_posts(id) ON DELETE RESTRICT,
    actor_id BIGINT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    kind SMALLINT NOT NULL CHECK (kind IN (1,2)),
    points BIGINT NOT NULL CHECK (points > 0),
    request_key TEXT,
    state SMALLINT NOT NULL DEFAULT 0 CHECK (state IN (0,1,2)),
    provider_id BIGINT UNIQUE,
    provider_no TEXT UNIQUE,
    provider_bad_debt BIGINT CHECK (provider_bad_debt BETWEEN 0 AND points),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    paid_at TIMESTAMPTZ,
    CHECK (
        (state=1 AND provider_id>0 AND provider_id IS NOT NULL AND provider_no IS NOT NULL AND
         provider_no<>'' AND provider_bad_debt IS NOT NULL AND paid_at IS NOT NULL)
        OR (state<>1 AND provider_id IS NULL AND provider_no IS NULL AND provider_bad_debt IS NULL AND paid_at IS NULL)
    )
);

CREATE UNIQUE INDEX anchor_post_order_single_pending ON anchor_post_orders (post_id,actor_id,kind) WHERE state=0;
CREATE UNIQUE INDEX anchor_post_order_single_unlock ON anchor_post_orders (post_id,actor_id) WHERE kind=1 AND state=1;
CREATE UNIQUE INDEX anchor_post_order_request ON anchor_post_orders (actor_id,request_key) WHERE request_key IS NOT NULL;
CREATE INDEX anchor_post_order_rank ON anchor_post_orders (post_id,kind,actor_id) WHERE state=1;
CREATE INDEX anchor_post_order_paid_actor ON anchor_post_orders (actor_id) WHERE state=1;
