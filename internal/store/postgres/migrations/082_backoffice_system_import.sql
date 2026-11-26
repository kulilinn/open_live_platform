CREATE TABLE backoffice_system_imports (
    area TEXT PRIMARY KEY CHECK (area IN ('admin','mcn')),
    summary JSONB NOT NULL,
    completed_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp()
);
