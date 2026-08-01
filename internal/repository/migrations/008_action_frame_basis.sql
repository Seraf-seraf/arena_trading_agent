ALTER TABLE action_requests
    ADD COLUMN based_on_captured_at TEXT NOT NULL DEFAULT '';

ALTER TABLE action_requests
    ADD COLUMN based_on_frame_digest TEXT NOT NULL DEFAULT '';

ALTER TABLE action_requests
    ADD COLUMN based_on_state TEXT NOT NULL DEFAULT '';

ALTER TABLE action_requests
    ADD COLUMN min_verification_confidence REAL NOT NULL DEFAULT 0;
