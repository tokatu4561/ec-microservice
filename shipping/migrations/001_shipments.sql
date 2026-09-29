CREATE TABLE IF NOT EXISTS shipments (
 order_id text PRIMARY KEY CHECK (order_id ~ '^[0-9a-f]{32}$'),
 items jsonb NOT NULL CHECK (jsonb_typeof(items)='array' AND jsonb_array_length(items) BETWEEN 1 AND 100),
 request_mode text NOT NULL CHECK (request_mode IN ('success','fail')),
 status text NOT NULL CHECK (status IN ('requested','failed','cancelled')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
