ALTER TABLE rooms ADD COLUMN IF NOT EXISTS history_after_id BIGINT;

-- Older schemas only recorded the last room update. Keep the suffix that
-- follows that update for online rooms and exclude all messages for closed rooms.
UPDATE rooms AS r SET history_after_id = COALESCE((
    SELECT max(m.id) FROM room_messages AS m
    WHERE m.room_id = r.id AND (NOT r.online OR m.created_at <= r.updated_at)
), 0) WHERE history_after_id IS NULL;

ALTER TABLE rooms ALTER COLUMN history_after_id SET DEFAULT 0,
    ALTER COLUMN history_after_id SET NOT NULL;

ALTER TABLE room_messages ADD COLUMN IF NOT EXISTS payload TEXT NOT NULL DEFAULT '';
