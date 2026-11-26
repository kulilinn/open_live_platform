CREATE TABLE finance_live_income (
    live_id BIGINT NOT NULL,
    streamer_id BIGINT NOT NULL,
    report_day DATE NOT NULL,
    streamer_sid TEXT NOT NULL,
    streamer_name TEXT NOT NULL,
    mcn_id BIGINT,
    mcn_name TEXT,
    started_at TIMESTAMPTZ NOT NULL,
    ended_at TIMESTAMPTZ,
    minutes BIGINT NOT NULL,
    diamond NUMERIC NOT NULL,
    PRIMARY KEY (report_day, live_id, streamer_id)
);

CREATE INDEX finance_live_income_filter ON finance_live_income(started_at, report_day DESC);
