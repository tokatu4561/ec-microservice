CREATE TABLE IF NOT EXISTS stocks (
 product_id bigint PRIMARY KEY CHECK(product_id>0),
 available bigint NOT NULL CHECK(available>=0 AND available<=9007199254740991)
);
CREATE TABLE IF NOT EXISTS reservations (
 order_id text PRIMARY KEY CHECK(order_id ~ '^[0-9a-f]{32}$'),
 items jsonb NOT NULL CHECK(jsonb_typeof(items)='array'),
 status text NOT NULL CHECK(status IN ('reserved','rejected','committed','released')),
 shortages jsonb NOT NULL DEFAULT '[]',
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE IF NOT EXISTS inventory_import (
 singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
 fingerprint text NOT NULL
);
