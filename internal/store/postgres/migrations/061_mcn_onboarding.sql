ALTER TABLE anchor_profiles ADD COLUMN agency_id BIGINT NOT NULL DEFAULT 0 CHECK(agency_id>=0);
ALTER TABLE backoffice_mcn_streamers ADD COLUMN invite_hash BYTEA,
    ADD COLUMN invite_until TIMESTAMPTZ,
    ADD COLUMN joined_at TIMESTAMPTZ,
    ADD COLUMN superseded BOOLEAN NOT NULL DEFAULT FALSE;
