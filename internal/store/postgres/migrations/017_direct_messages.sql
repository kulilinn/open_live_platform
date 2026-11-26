CREATE TABLE IF NOT EXISTS dm_conversations (
    id BIGSERIAL PRIMARY KEY,
    fan_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    anchor_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    member_low BIGINT GENERATED ALWAYS AS (LEAST(fan_id, anchor_id)) STORED,
    member_high BIGINT GENERATED ALWAYS AS (GREATEST(fan_id, anchor_id)) STORED,
    metadata JSONB NOT NULL,
    last_message TEXT NOT NULL DEFAULT '',
    last_message_time TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    create_time TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    can_message BOOLEAN NOT NULL DEFAULT true,
    hidden_by_fan BOOLEAN NOT NULL DEFAULT false,
    hidden_by_anchor BOOLEAN NOT NULL DEFAULT false,
    UNIQUE (member_low, member_high),
    CHECK (fan_id <> anchor_id)
);
CREATE INDEX IF NOT EXISTS dm_conversations_fan_idx ON dm_conversations(fan_id, last_message_time DESC, id DESC);
CREATE INDEX IF NOT EXISTS dm_conversations_anchor_idx ON dm_conversations(anchor_id, last_message_time DESC, id DESC);

CREATE TABLE IF NOT EXISTS dm_messages (
    id BIGSERIAL PRIMARY KEY,
    conversation_id BIGINT NOT NULL REFERENCES dm_conversations(id) ON DELETE CASCADE,
    sender_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    receiver_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    message_type SMALLINT NOT NULL,
    content TEXT NOT NULL,
    status SMALLINT NOT NULL DEFAULT 1,
    create_time TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    CHECK (sender_id <> receiver_id),
    CHECK (status IN (1, 2, 3))
);
CREATE INDEX IF NOT EXISTS dm_messages_history_idx ON dm_messages(conversation_id, create_time DESC, id DESC);
CREATE INDEX IF NOT EXISTS dm_messages_unread_idx ON dm_messages(receiver_id, conversation_id) WHERE status IN (1, 2);
