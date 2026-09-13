BEGIN;
CREATE TABLE IF NOT EXISTS orders (
    id text PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    product_id bigint NOT NULL REFERENCES products(id),
    product_name text NOT NULL,
    quantity integer NOT NULL CHECK (quantity > 0),
    price_yen bigint NOT NULL CHECK (price_yen >= 0),
    status text NOT NULL CHECK (status IN ('shipping_requested', 'failed')),
    failure_reason text,
    payment_status text NOT NULL,
    shipping_status text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (
        (status = 'shipping_requested' AND failure_reason IS NULL AND payment_status = 'succeeded' AND shipping_status = 'requested')
        OR
        (status = 'failed' AND failure_reason IS NOT NULL AND (
            (failure_reason = 'out_of_stock' AND payment_status = 'not_started' AND shipping_status = 'not_started')
            OR (failure_reason = 'payment_failed' AND payment_status = 'failed' AND shipping_status = 'not_started')
            OR (failure_reason = 'shipping_failed' AND payment_status = 'cancelled' AND shipping_status = 'failed')
        ))
    )
);
COMMIT;
