ALTER TABLE date_hosts ADD COLUMN recommended_at TIMESTAMPTZ;

UPDATE date_hosts
SET recommended_at=(state->>'AcceptUntil')::timestamptz - interval '5 seconds'
WHERE state->>'Accepting'='true' AND COALESCE(state->>'AcceptUntil','')>'0001';

CREATE INDEX date_hosts_recommended_idx ON date_hosts(recommended_at DESC,anchor_id)
    WHERE recommended_at IS NOT NULL;
