BEGIN;
SELECT pg_advisory_xact_lock(2026091705);
CREATE TABLE IF NOT EXISTS payments (
    order_id text PRIMARY KEY CHECK (order_id ~ '^[0-9a-f]{32}$'),
    amount_yen bigint NOT NULL CHECK (amount_yen BETWEEN 0 AND 9007199254740991),
    status text NOT NULL CHECK (status IN ('succeeded', 'failed', 'cancelled')),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);

ALTER TABLE payments ADD COLUMN IF NOT EXISTS request_mode text;
UPDATE payments SET request_mode=CASE WHEN status='failed' THEN 'fail' ELSE 'success' END WHERE request_mode IS NULL;
ALTER TABLE payments ALTER COLUMN request_mode SET NOT NULL;
COMMIT;
