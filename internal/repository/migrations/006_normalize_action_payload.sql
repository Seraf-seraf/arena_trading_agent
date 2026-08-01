UPDATE action_requests
SET action_json = '{}'
WHERE length(action_json) = 0;
