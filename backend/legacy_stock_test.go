package main

import (
	"context"
	"github.com/jackc/pgx/v5"
)

// 学習2の比較用。通常のアプリに含めない。
// 呼出前に全商品のロックを取得済み。在庫予約と解除に使う。
func adjustOrderStock(ctx context.Context, tx pgx.Tx, items []OrderItem, direction int32) error {
	ids := make([]int64, len(items))
	deltas := make([]int32, len(items))
	for i, item := range items {
		ids[i], deltas[i] = item.ProductID, int32(item.Quantity)*direction
	}
	_, err := tx.Exec(ctx, `UPDATE products p SET stock=p.stock+i.delta
		FROM unnest($1::bigint[],$2::integer[]) AS i(product_id,delta) WHERE p.id=i.product_id`, ids, deltas)
	return err
}
