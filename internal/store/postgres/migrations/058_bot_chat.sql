CREATE TABLE bot_chat_messages (
    id text COLLATE "C" PRIMARY KEY,
    member_low bigint NOT NULL,
    member_high bigint NOT NULL,
    message jsonb NOT NULL
);
CREATE INDEX bot_chat_page ON bot_chat_messages (member_low, member_high, id);
