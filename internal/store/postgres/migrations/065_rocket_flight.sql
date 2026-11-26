ALTER TABLE room_gift_effects
    ADD COLUMN motion_at TIMESTAMPTZ,
    ADD COLUMN distance BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN attenuation INTEGER NOT NULL DEFAULT 0 CHECK (attenuation >= 0);

UPDATE room_gift_effects SET motion_at=start_at WHERE effect=6 AND start_at IS NOT NULL;

CREATE INDEX room_rocket_finalization_idx ON room_gift_effects(end_at,room_id)
    WHERE effect=6 AND final_result IS NULL AND start_at IS NOT NULL;

ALTER TABLE rocket_launch_amounts ADD COLUMN attenuation INTEGER NOT NULL DEFAULT 0 CHECK (attenuation>=0);
