BEGIN;
SELECT pg_advisory_xact_lock(2026091203);
CREATE TABLE IF NOT EXISTS order_items (
    order_id text NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    product_id bigint NOT NULL REFERENCES products(id),
    product_name text NOT NULL,
    quantity integer NOT NULL CHECK (quantity > 0),
    price_yen bigint NOT NULL CHECK (price_yen >= 0),
    stock_shortage boolean NOT NULL DEFAULT false,
    PRIMARY KEY (order_id, product_id)
);
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'orders' AND column_name = 'product_id') THEN
        INSERT INTO order_items (order_id,product_id,product_name,quantity,price_yen,stock_shortage)
        SELECT id,product_id,product_name,quantity,price_yen,COALESCE(failure_reason = 'out_of_stock', false) FROM orders
        ON CONFLICT (order_id,product_id) DO NOTHING;
        ALTER TABLE orders DROP COLUMN product_id, DROP COLUMN product_name, DROP COLUMN quantity, DROP COLUMN price_yen;
    END IF;
END $$;
CREATE TABLE IF NOT EXISTS carts (
    token_hash text PRIMARY KEY CHECK (token_hash ~ '^[0-9a-f]{64}$'),
    version bigint NOT NULL DEFAULT 0 CHECK (version >= 0),
    expires_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP + interval '30 days',
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_order_id text REFERENCES orders(id)
);
CREATE TABLE IF NOT EXISTS cart_items (
    cart_hash text NOT NULL REFERENCES carts(token_hash) ON DELETE CASCADE,
    product_id bigint NOT NULL REFERENCES products(id),
    quantity integer NOT NULL CHECK (quantity > 0),
    PRIMARY KEY (cart_hash, product_id)
);
COMMIT;
