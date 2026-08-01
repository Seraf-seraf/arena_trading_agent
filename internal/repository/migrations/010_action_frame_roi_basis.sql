ALTER TABLE action_requests
    ADD COLUMN frame_basis_json BLOB NOT NULL DEFAULT '[]';
