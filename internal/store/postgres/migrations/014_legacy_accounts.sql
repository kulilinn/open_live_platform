ALTER TABLE users ADD COLUMN livemedia_imported BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN profile JSONB;
