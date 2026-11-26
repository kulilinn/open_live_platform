ALTER TABLE rooms ADD COLUMN IF NOT EXISTS banner TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS tag TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS description TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS offline_info TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS plan JSONB NOT NULL DEFAULT '[]',
    ADD COLUMN IF NOT EXISTS gifts JSONB NOT NULL DEFAULT '[]',
    ADD COLUMN IF NOT EXISTS trans_enabled BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS anchor_trans_lang TEXT NOT NULL DEFAULT '';

ALTER TABLE users ADD COLUMN IF NOT EXISTS trans_enabled BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS current_viewer_room BIGINT;

ALTER TABLE room_members ADD COLUMN IF NOT EXISTS trans_lang TEXT NOT NULL DEFAULT 'en';

UPDATE room_members AS m SET trans_lang=COALESCE(NULLIF(u.trans_lang,''),'en')
FROM users AS u WHERE u.id=m.user_id AND u.id<>m.room_id;

UPDATE rooms SET anchor_trans_lang=COALESCE(NULLIF(trans_lang,''),'en') WHERE online AND anchor_joined;

UPDATE users AS u SET current_viewer_room=(
    SELECT room_id FROM room_members WHERE user_id=u.id AND room_id<>u.id ORDER BY joined_at DESC, room_id DESC LIMIT 1
);
