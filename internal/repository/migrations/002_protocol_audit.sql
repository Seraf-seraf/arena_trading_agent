ALTER TABLE action_requests
    ADD COLUMN agent_id TEXT NOT NULL DEFAULT '';

ALTER TABLE action_requests
    ADD COLUMN expected_width INTEGER NOT NULL DEFAULT 0;

ALTER TABLE action_requests
    ADD COLUMN expected_height INTEGER NOT NULL DEFAULT 0;

ALTER TABLE action_requests
    ADD COLUMN expected_dpi_percent INTEGER NOT NULL DEFAULT 0;

ALTER TABLE action_requests
    ADD COLUMN delta INTEGER NOT NULL DEFAULT 0;

CREATE INDEX action_requests_agent_idx
    ON action_requests(agent_id, requested_at DESC);

ALTER TABLE action_results
    ADD COLUMN message_id TEXT NOT NULL DEFAULT '';

ALTER TABLE action_results
    ADD COLUMN correlation_id TEXT NOT NULL DEFAULT '';

ALTER TABLE action_results
    ADD COLUMN agent_id TEXT NOT NULL DEFAULT '';

ALTER TABLE action_results
    ADD COLUMN received_at TEXT NOT NULL DEFAULT '';

CREATE INDEX action_results_message_idx
    ON action_results(message_id);

CREATE INDEX action_results_agent_idx
    ON action_results(agent_id, received_at DESC);
