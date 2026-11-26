ALTER TABLE relay_tickets ADD COLUMN app_user BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE relay_tickets ADD COLUMN is_debug BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE relay_tickets DROP CONSTRAINT relay_tickets_check;
ALTER TABLE relay_tickets ADD CONSTRAINT relay_tickets_login_check CHECK (
    (app_user AND opaque_token IS NULL AND family_id = 0 AND issuer = '')
    OR (NOT app_user AND opaque_token IS NULL AND family_id <> 0 AND issuer <> '')
    OR (NOT app_user AND opaque_token IS NOT NULL AND family_id = 0 AND issuer = '')
);

CREATE TABLE relay_control_grants (
    app_key TEXT NOT NULL,
    controller_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    controlled_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    duplex BOOLEAN NOT NULL,
    granted_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (app_key, controller_id, controlled_id),
    CHECK (controller_id <> controlled_id)
);

CREATE INDEX relay_control_grants_controlled_idx ON relay_control_grants(controlled_id);
