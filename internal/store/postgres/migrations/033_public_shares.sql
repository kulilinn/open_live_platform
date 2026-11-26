CREATE TABLE public_shares(
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    content TEXT NOT NULL,
    photo TEXT NOT NULL,
    comments TEXT NOT NULL DEFAULT '[]',
    likes INTEGER NOT NULL DEFAULT 0,
    status SMALLINT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX public_shares_recent ON public_shares(created_at DESC,id DESC) WHERE status>0;
CREATE INDEX public_shares_owner ON public_shares(user_id,created_at DESC) WHERE status>0;
