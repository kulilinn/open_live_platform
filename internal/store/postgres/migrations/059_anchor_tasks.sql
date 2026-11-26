CREATE TABLE anchor_task_events (
    anchor_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    task_day DATE NOT NULL,
    target SMALLINT NOT NULL CHECK (target BETWEEN 0 AND 4),
    event_key TEXT NOT NULL,
    quantity BIGINT NOT NULL CHECK (quantity>0),
    occurred_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (anchor_id,task_day,target,event_key)
);

CREATE TABLE anchor_task_rewards (
    anchor_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    task_day DATE NOT NULL,
    task_id BIGINT NOT NULL,
    stars INTEGER NOT NULL CHECK (stars>0),
    awarded_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (anchor_id,task_day,task_id)
);
CREATE INDEX anchor_task_rewards_chronological ON anchor_task_rewards (anchor_id,awarded_at);
