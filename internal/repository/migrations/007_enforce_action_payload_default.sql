CREATE TRIGGER action_requests_normalize_empty_payload_after_insert
AFTER INSERT ON action_requests
WHEN length(NEW.action_json) = 0
BEGIN
    UPDATE action_requests
    SET action_json = '{}'
    WHERE id = NEW.id;
END;

CREATE TRIGGER action_requests_normalize_empty_payload_after_update
AFTER UPDATE OF action_json ON action_requests
WHEN length(NEW.action_json) = 0
BEGIN
    UPDATE action_requests
    SET action_json = '{}'
    WHERE id = NEW.id;
END;
