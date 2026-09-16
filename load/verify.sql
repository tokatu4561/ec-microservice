\set ON_ERROR_STOP on
-- k6が生成した /results/verify.sql から値を受け取る。負荷終了後に実行する。
WITH result AS (
  SELECT
    (SELECT count(*) FROM orders) AS orders,
    (SELECT count(*) FROM orders WHERE status='shipping_requested') AS success,
    (SELECT count(*) FROM orders WHERE status='failed' AND failure_reason='out_of_stock') AS shortage,
    (SELECT count(*) FROM order_items) AS items,
    (SELECT count(*) FROM products p LEFT JOIN
      (SELECT i.product_id, sum(i.quantity) AS sold FROM order_items i
       JOIN orders o ON o.id=i.order_id WHERE o.status='shipping_requested'
       GROUP BY i.product_id) s ON s.product_id=p.id
      WHERE p.stock<0 OR p.stock+COALESCE(s.sold,0)<>1000000) AS invalid_products,
    (SELECT count(*) FROM orders o WHERE
      (SELECT count(*) FROM order_items i WHERE i.order_id=o.id)<>:expected_items
      OR (o.status='shipping_requested' AND (o.failure_reason IS NOT NULL
        OR o.payment_status<>'succeeded' OR o.shipping_status<>'requested'))) AS invalid_orders,
    (SELECT count(*) FROM order_items i JOIN products p ON p.id=i.product_id
      WHERE i.quantity<>1 OR i.price_yen<>500 OR i.product_name<>p.name) AS invalid_items
)
SELECT *, (current_database()='ec_load' AND :'client_checks_passed'::boolean
  AND orders=:expected_success+:expected_shortage
  AND success=:expected_success AND shortage=:expected_shortage
  AND items=orders*:expected_items
  AND invalid_products=0 AND invalid_orders=0 AND invalid_items=0) AS passed
FROM result
\gset result_
\echo orders=:result_orders success=:result_success shortage=:result_shortage items=:result_items
\echo invalid_products=:result_invalid_products invalid_orders=:result_invalid_orders invalid_items=:result_invalid_items
\echo client_checks_passed=:client_checks_passed passed=:result_passed
\if :result_passed
  \echo PASS: response counts, saved orders/items and stock agree.
\else
  \echo FAIL: do not treat this run as a correct performance result.
  DO $$ BEGIN RAISE EXCEPTION 'load verification failed'; END $$;
\endif
