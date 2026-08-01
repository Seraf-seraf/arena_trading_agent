ALTER TABLE action_results
    ADD COLUMN not_sent INTEGER NOT NULL DEFAULT 0
    CHECK (not_sent IN (0, 1));

ALTER TABLE action_results
    ADD COLUMN retry_safe INTEGER NOT NULL DEFAULT 0
    CHECK (retry_safe IN (0, 1));
