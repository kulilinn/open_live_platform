-- Connections from before this migration have one lease period to reconnect.
ALTER TABLE room_anchor_connections ADD COLUMN lease_until TIMESTAMPTZ NOT NULL
    DEFAULT (statement_timestamp() + INTERVAL '30 seconds');

CREATE INDEX room_anchor_connections_lease_until_idx ON room_anchor_connections(lease_until);
