ALTER TABLE users ADD COLUMN IF NOT EXISTS trans_lang TEXT NOT NULL DEFAULT '';
ALTER TABLE rooms ADD COLUMN IF NOT EXISTS trans_lang TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS anchor_joined BOOLEAN NOT NULL DEFAULT false;

-- Older releases recorded the anchor in room_members after an explicit join.
UPDATE rooms SET anchor_joined = true WHERE online AND EXISTS (
    SELECT 1 FROM room_members WHERE room_id = rooms.id AND user_id = rooms.owner_id
);
