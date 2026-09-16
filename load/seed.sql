\set ON_ERROR_STOP on
DO $$ BEGIN
  IF current_database() <> 'ec_load' THEN RAISE EXCEPTION 'requires isolated ec_load database'; END IF;
END $$;
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
TRUNCATE cart_items, carts, order_items, orders, products RESTART IDENTITY;
INSERT INTO products (id,name,price_yen,stock)
SELECT n, '負荷試験商品' || n, 500, 1000000 FROM generate_series(1,10000) n;
SELECT setval(pg_get_serial_sequence('products','id'),10000);
VACUUM ANALYZE;
SELECT pg_stat_statements_reset();
