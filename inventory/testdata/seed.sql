-- 専用テストDBのみ。通常環境はOrderからオフライン移行する。
INSERT INTO stocks(product_id,available) VALUES(1,10),(2,20),(3,0);
INSERT INTO inventory_import(singleton,fingerprint) VALUES(true,'test');
