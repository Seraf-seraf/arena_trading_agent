ALTER TABLE action_requests
    ADD COLUMN action_json BLOB NOT NULL DEFAULT '{}';

ALTER TABLE action_results
    ADD COLUMN verification_confidence REAL NOT NULL DEFAULT 0;
