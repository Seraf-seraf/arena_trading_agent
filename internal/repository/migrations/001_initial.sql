CREATE TABLE observations (
    frame_id       INTEGER PRIMARY KEY,
    state          TEXT NOT NULL,
    elements_json  BLOB NOT NULL,
    values_json    BLOB NOT NULL,
    confidence     REAL NOT NULL,
    created_at     TEXT NOT NULL
);

CREATE INDEX observations_created_at_idx
    ON observations(created_at DESC, frame_id DESC);
CREATE INDEX observations_state_created_at_idx
    ON observations(state, created_at DESC);

CREATE TABLE market_quotes (
    sequence       INTEGER PRIMARY KEY AUTOINCREMENT,
    item_id        TEXT NOT NULL,
    buy_price      INTEGER NOT NULL,
    sale_price     INTEGER NOT NULL,
    observed_at    TEXT NOT NULL,
    confidence     REAL NOT NULL
);

CREATE INDEX market_quotes_item_observed_idx
    ON market_quotes(item_id, observed_at DESC, sequence DESC);
CREATE INDEX market_quotes_observed_idx
    ON market_quotes(observed_at DESC, sequence DESC);

CREATE TABLE trade_executions (
    id               TEXT PRIMARY KEY,
    opportunity_id   TEXT NOT NULL,
    status           TEXT NOT NULL,
    current_step     INTEGER NOT NULL,
    reserved         INTEGER NOT NULL,
    started_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    failure          TEXT NOT NULL
);

CREATE INDEX trade_executions_updated_idx
    ON trade_executions(updated_at DESC, id DESC);
CREATE INDEX trade_executions_opportunity_idx
    ON trade_executions(opportunity_id, updated_at DESC);
CREATE INDEX trade_executions_status_idx
    ON trade_executions(status, updated_at DESC);

CREATE TABLE trade_execution_history (
    sequence         INTEGER PRIMARY KEY AUTOINCREMENT,
    execution_id     TEXT NOT NULL,
    opportunity_id   TEXT NOT NULL,
    status           TEXT NOT NULL,
    current_step     INTEGER NOT NULL,
    reserved         INTEGER NOT NULL,
    started_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    failure          TEXT NOT NULL
);

CREATE INDEX trade_execution_history_execution_idx
    ON trade_execution_history(execution_id, sequence DESC);

CREATE TABLE action_requests (
    id               TEXT PRIMARY KEY,
    session_id       TEXT NOT NULL,
    based_on_frame   INTEGER NOT NULL,
    expected_state   TEXT NOT NULL,
    deadline         TEXT NOT NULL,
    action_kind      TEXT NOT NULL,
    point_x          REAL,
    point_y          REAL,
    value            TEXT NOT NULL,
    requested_at     TEXT NOT NULL
);

CREATE INDEX action_requests_requested_idx
    ON action_requests(requested_at DESC, id DESC);
CREATE INDEX action_requests_session_idx
    ON action_requests(session_id, requested_at DESC);

CREATE TABLE action_results (
    sequence         INTEGER PRIMARY KEY AUTOINCREMENT,
    action_id        TEXT NOT NULL,
    success          INTEGER NOT NULL,
    result_frame     INTEGER NOT NULL,
    result_state     TEXT NOT NULL,
    error            TEXT NOT NULL,
    completed_at     TEXT NOT NULL,
    FOREIGN KEY (action_id) REFERENCES action_requests(id)
);

CREATE INDEX action_results_action_idx
    ON action_results(action_id, sequence DESC);

CREATE TABLE agent_events (
    id               TEXT PRIMARY KEY,
    session_id       TEXT NOT NULL,
    agent_id         TEXT NOT NULL,
    kind             TEXT NOT NULL,
    severity         TEXT NOT NULL,
    message          TEXT NOT NULL,
    payload          BLOB,
    created_at       TEXT NOT NULL
);

CREATE INDEX agent_events_created_idx
    ON agent_events(created_at DESC, id DESC);
CREATE INDEX agent_events_session_idx
    ON agent_events(session_id, created_at DESC);
CREATE INDEX agent_events_agent_idx
    ON agent_events(agent_id, created_at DESC);
CREATE INDEX agent_events_kind_idx
    ON agent_events(kind, created_at DESC);
