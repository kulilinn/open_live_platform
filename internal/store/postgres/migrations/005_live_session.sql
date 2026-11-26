CREATE SEQUENCE IF NOT EXISTS room_live_id_seq AS BIGINT START WITH 4294967296;

ALTER TABLE rooms ADD COLUMN IF NOT EXISTS live_id BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS live_started_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS live_ended_at TIMESTAMPTZ;

-- Older rooms have no explicit start time. Use the last known update for
-- broadcasts still online at migration time.
UPDATE rooms SET live_id = nextval('room_live_id_seq'), live_started_at = updated_at
WHERE online AND live_id = 0;
