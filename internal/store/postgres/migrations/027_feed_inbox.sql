CREATE TABLE message_inbox(
    owner_id BIGINT NOT NULL REFERENCES users(id),
    peer_id BIGINT NOT NULL REFERENCES users(id),
    data TEXT NOT NULL,
    unreads INTEGER NOT NULL,
    hidden BOOLEAN NOT NULL DEFAULT false,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY(owner_id,peer_id)
);
CREATE INDEX message_inbox_by_owner ON message_inbox(owner_id,updated_at DESC) WHERE NOT hidden;
CREATE TABLE message_config(
    user_id BIGINT PRIMARY KEY REFERENCES users(id),
    dating BOOLEAN NOT NULL,
    dated BOOLEAN NOT NULL,
    welcome TEXT NOT NULL DEFAULT ''
);
CREATE INDEX dm_messages_by_receiver ON dm_messages(receiver_id,sender_id,create_time DESC);
