CREATE TABLE room_moderation_mail_templates (
    key text PRIMARY KEY,
    texts jsonb NOT NULL
);

CREATE TABLE room_moderation_mail (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    delivery_key uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    room_id bigint NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    action text NOT NULL CHECK(action IN ('streamer_disable','streamer_enable')),
    email text NOT NULL,
    display_name text NOT NULL,
    locale text NOT NULL CHECK(locale IN ('zh_cn','zh_tw','en_us','ja_jp')),
    ban_until bigint NOT NULL,
    reasons text[] NOT NULL,
    template text,
    fields jsonb,
    attempts integer NOT NULL DEFAULT 0 CHECK(attempts>=0),
    next_attempt timestamptz NOT NULL DEFAULT statement_timestamp(),
    lease_id text,
    lease_until timestamptz,
    sent_at timestamptz
);
CREATE INDEX room_moderation_mail_ready ON room_moderation_mail(next_attempt,id) WHERE sent_at IS NULL;
CREATE INDEX room_moderation_mail_order ON room_moderation_mail(room_id,id) WHERE sent_at IS NULL;
