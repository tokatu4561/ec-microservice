BEGIN;
SELECT pg_advisory_xact_lock(2026091704);
DO $$ BEGIN
IF NOT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='orders' AND column_name='inventory_status') THEN
ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_flow_check;
ALTER TABLE orders ADD CONSTRAINT orders_flow_check CHECK (
    (status='processing' AND failure_reason IS NULL AND payment_status='pending' AND shipping_status='not_started')
    OR (status='processing' AND failure_reason IS NULL AND payment_status='succeeded' AND shipping_status='pending')
    OR (status='cancel_pending' AND failure_reason IS NULL AND payment_status='succeeded' AND shipping_status='failed')
    OR (status='shipping_requested' AND failure_reason IS NULL AND payment_status='succeeded' AND shipping_status='requested')
    OR (status='failed' AND failure_reason IS NOT NULL AND (
        (failure_reason='out_of_stock' AND payment_status='not_started' AND shipping_status='not_started')
        OR (failure_reason='payment_failed' AND payment_status='failed' AND shipping_status='not_started')
        OR (failure_reason='shipping_failed' AND payment_status='cancelled' AND shipping_status='failed')
    ))
);
END IF; END $$;
COMMIT;
