ALTER TABLE rooms ADD COLUMN IF NOT EXISTS anchor_away_until TIMESTAMPTZ;

-- Existing away rooms get the default grace period from migration time.
UPDATE rooms
SET anchor_away_until = (SELECT clock_timestamp() + INTERVAL '5 minutes')
WHERE online AND anchor_away AND anchor_away_until IS NULL;

CREATE INDEX IF NOT EXISTS rooms_anchor_away_until_idx ON rooms(anchor_away_until)
WHERE online AND anchor_away AND anchor_away_until IS NOT NULL;
