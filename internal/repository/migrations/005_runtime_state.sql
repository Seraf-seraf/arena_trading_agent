CREATE TABLE trade_quotes (
    sequence          INTEGER PRIMARY KEY AUTOINCREMENT,
    item_id           TEXT NOT NULL,
    purchase_price    INTEGER NOT NULL,
    sale_price        INTEGER NOT NULL,
    sale_commission   INTEGER NOT NULL,
    listing_fee       INTEGER NOT NULL,
    observed_at       TEXT NOT NULL,
    confidence        REAL NOT NULL,
    liquidity_score   REAL NOT NULL,
    price_volatility  REAL NOT NULL,
    resale_known      INTEGER NOT NULL
);

CREATE INDEX trade_quotes_item_observed_idx
    ON trade_quotes(item_id, observed_at DESC, sequence DESC);
CREATE INDEX trade_quotes_observed_idx
    ON trade_quotes(observed_at DESC, sequence DESC);

CREATE TABLE runtime_records (
    key         TEXT PRIMARY KEY,
    kind        TEXT NOT NULL,
    payload     BLOB NOT NULL,
    updated_at  TEXT NOT NULL
);

CREATE INDEX runtime_records_kind_updated_idx
    ON runtime_records(kind, updated_at DESC, key ASC);
