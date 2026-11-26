CREATE SEQUENCE local_notice_ids AS BIGINT MINVALUE -9000000000000000000 MAXVALUE -1;

ALTER TABLE notices ADD COLUMN source_key TEXT;
CREATE UNIQUE INDEX notices_source_key ON notices (receive_id,source_key) WHERE source_key IS NOT NULL;

ALTER TABLE anchor_posts ADD COLUMN audit_request_id TEXT NOT NULL DEFAULT '';
ALTER TABLE anchor_posts ADD COLUMN manual_audit_request_id TEXT NOT NULL DEFAULT '';
ALTER TABLE anchor_post_comments ADD COLUMN manual_audit_request_id TEXT NOT NULL DEFAULT '';
