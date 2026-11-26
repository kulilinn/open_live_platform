ALTER TABLE anchor_kyc_applicants
    ALTER COLUMN applicant_id DROP NOT NULL,
    ADD COLUMN submitted JSONB NOT NULL DEFAULT '{}',
    ADD COLUMN status SMALLINT NOT NULL DEFAULT 0,
    ADD COLUMN created_at TIMESTAMPTZ NOT NULL DEFAULT now();

-- Preserve a cancellation across an import retry without retaining the identity form itself.
CREATE TABLE anchor_review_cancellations (
    user_id BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE
);

DROP INDEX subscription_single_native_free_plan;
CREATE UNIQUE INDEX subscription_single_native_free_plan ON subscription_plans(anchor_id)
    WHERE plan_type=2 AND NOT livemedia_imported AND NOT deleted;
