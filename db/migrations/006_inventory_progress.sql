BEGIN;
SELECT pg_advisory_xact_lock(2026091704);
ALTER TABLE orders ADD COLUMN IF NOT EXISTS inventory_status text NOT NULL DEFAULT 'legacy';
ALTER TABLE order_progress ADD COLUMN IF NOT EXISTS phase text NOT NULL DEFAULT 'legacy';
ALTER TABLE order_progress ADD COLUMN IF NOT EXISTS failure_reason text;
CREATE TABLE IF NOT EXISTS inventory_cutover(singleton boolean PRIMARY KEY CHECK(singleton),fingerprint text NOT NULL);
ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_flow_check;
ALTER TABLE orders ADD CONSTRAINT orders_flow_check CHECK (
 (status IN ('processing','cancel_pending','release_pending') AND failure_reason IS NULL
  AND payment_status IN ('not_started','pending','succeeded','failed','cancelled')
  AND shipping_status IN ('not_started','pending','requested','failed'))
 OR (status='shipping_requested' AND failure_reason IS NULL AND payment_status='succeeded' AND shipping_status='requested')
 OR (status='failed' AND failure_reason IS NOT NULL AND (
  (failure_reason='out_of_stock' AND payment_status='not_started' AND shipping_status='not_started')
  OR (failure_reason='payment_failed' AND payment_status='failed' AND shipping_status='not_started')
  OR (failure_reason='shipping_failed' AND payment_status='cancelled' AND shipping_status='failed')
 ))
);
ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_inventory_status_check;
ALTER TABLE orders ADD CONSTRAINT orders_inventory_status_check CHECK(inventory_status IN ('legacy','pending','reserved','commit_pending','release_pending','committed','released','rejected'));
ALTER TABLE order_progress DROP CONSTRAINT IF EXISTS order_progress_phase_check;
ALTER TABLE order_progress ADD CONSTRAINT order_progress_phase_check CHECK(phase IN ('legacy','inventory_pending','payment_pending','shipping_pending','inventory_commit_pending','payment_cancel_pending','inventory_release_pending','finalize_success','finalize_failure','done'));
COMMIT;
