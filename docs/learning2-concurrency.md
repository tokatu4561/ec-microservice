# 学習2・最初の実験：同時注文と悲観ロック

[Issue #3](https://github.com/tokatu4561/ec-microservice/issues/3)の最初の実装単位。
目的は、同じ商品を同時に注文したときの在庫の正しさと、ロックによる待機を実際のコードとDBで確認すること。
実装は[テスト](../backend/concurrency_test.go)にあり、既存の[注文処理](../backend/orders.go)を呼び出す。

## 実験前にコードを追う

`CreateOrder`はRead Committedでトランザクションを開始し、`createOrderTx`を呼ぶ。
商品をID順に`SELECT ... FOR UPDATE`でロックしてから在庫を判断し、在庫更新と注文・明細保存を同じトランザクションで行う。
`Commit`が成功してから注文を返す。複数商品のカートも`createOrderTx`を共用する。

例えば、在庫1に対する数量1の注文A・Bは、次のように進む。

```text
注文A: BEGIN → 商品をロック（在庫1）→ 在庫0へ更新 → 成功注文を保存 → COMMIT
注文B: BEGIN → 同じ商品のロックを待つ ────────────────────────┘
              → ロック取得（更新後の在庫0）→ 在庫不足の注文を保存 → COMMIT
```

同じ商品への競合する更新・ロック取得を待たせるのが今回の悲観ロック。
先に在庫を確認できた注文がコミットすると、待っていた注文は更新後の行を取得する。
通常の`SELECT`まで同じ行ロックで止まるわけではない。
根拠：[PostgreSQL 18の行ロック](https://www.postgresql.org/docs/18/explicit-locking.html#LOCKING-ROWS)。

在庫不足は想定する業務結果で、失敗注文を保存してコミットする。DB障害による未完了と区別する。
今回の実験はHTTPを通さずGoの注文処理を直接呼ぶため、HTTPレイテンシ・HTTPエラー率は測らない。

## 再実行する

リポジトリのルートで実行する。Docker Desktopを起動し、`docker`が見つからない場合は以下を先に実行する。

```sh
export PATH="/Applications/Docker.app/Contents/Resources/bin:$PATH"
```

```sh
# 学習用の2実験だけ。-count=1でGoのテスト結果キャッシュを使わない。
docker compose --profile test run --build --rm api-test \
  go test -mod=readonly -race -count=1 -v \
  -run '^(TestConcurrentOrdersPostgres|TestOrderLockWaitPostgres)$' ./...

# 既存の注文・カート・DB移行を含む全テストと静的検査
docker compose --profile test run --build --rm api-test
```

Composeの`api-test`から独立した`test-db`へ接続し、実験ごとに商品を作って終了時に注文・商品を削除する。
通常画面の`db`とその名前付きボリュームは使わない。`TEST_DATABASE_URL`を通常利用するDBへ向けないこと。
テストDBはtmpfsで、停止すると中身が破棄される。他のテストが使っていないことを確認し、必要なら終了後に止める。

```sh
docker compose --profile test stop test-db
```

`TEST_DATABASE_URL`なしの`go test`ではDBテストがスキップされるため、上記Composeコマンドを使う。

## 実験1：在庫10へ20件の同時注文

`TestConcurrentOrdersPostgres`は在庫1・注文2件の既存ケースに加え、在庫10・注文20件を検証する。
全goroutineが開始の合図を待つ状態まで準備し、共通のchannelを閉じて注文を開始する。
同時に開始可能にする仕組みであり、DBの20接続が厳密に同時実行する保証ではない。
実際にはGoのスケジューラ、接続プール上限、DBのロックで処理が順に進む。

| 条件・検証対象 | 小さい既存ケース | 学習2のケース |
| --- | --- | --- |
| 対象 | 同一商品1種類 | 同一商品1種類 |
| 初期在庫 | 1 | 10 |
| 同時に開始する注文数 | 2 | 20 |
| 1注文の数量 | 1 | 1 |
| 模擬決済・配送 | 成功 | 成功 |
| 成功注文 | 1 | 10 |
| 在庫不足の注文 | 1 | 10 |
| 残在庫 | 0 | 0 |
| 保存した注文・明細 | 各2 | 各20 |
| 成功注文の数量合計 | 1 | 10 |

戻り値の分類だけでなく、DBの`orders`と`order_items`を集計し、成功数量が初期在庫を超えていないことを確認する。
予期しないDBエラー、重複注文ID、想定外の状態もテスト失敗になる。

最後の集計ログで`success`、`out_of_stock`、`remaining_stock`、`saved_orders`、`sold_quantity`を照合する。
`pool_max_conns`はこのテストの注文用プールの上限で、通常稼働しているAPIの実測接続数ではない。

- `batch_elapsed`：開始の合図から全結果を回収するまで。
- `order_total_min/max`：各`CreateOrder`呼出しから復帰までの最小・最大時間。

注文所要時間には接続取得、SQL、ロック待ち、コミットなどが含まれる。ロック待ち時間だけを表す値ではない。
20件の1回の実験でp95/p99や処理能力は評価しない。継続負荷、事前に決める許容値、詳細な時間の内訳は次の単位で扱う。

## 実験2：ロック待ちをDBで確かめる

`TestOrderLockWaitPostgres`は在庫1の商品で以下を行う。

1. 制御用接続で`BEGIN`し、商品の`SELECT ... FOR UPDATE`を実行する。商品は更新しない。
2. 別のgoroutineから実際の`CreateOrder`を開始する。
3. 観測用接続から`pg_stat_activity`と`pg_blocking_pids()`を調べる。
4. 制御用接続が原因の`wait_event_type=Lock`を確認し、さらに200msロックを保持する。
5. 制御用トランザクションをコミットし、注文が再開・成功して在庫0と注文1件が保存されることを確認する。

制御用・観測用の2接続は注文プールの外に作る。観測自体が注文の接続プール待ちに巻き込まれないようにするため。
この2接続は実験用であり、通常アプリの構成変更ではない。
待機の確認は10ms間隔、実験のcontextには10秒の期限を設ける。所要時間の速さを合否には使わない。

観測SQLの要点は以下。`$1`に制御用接続のPostgreSQLバックエンドPIDを渡す。

```sql
SELECT pid, wait_event, query
FROM pg_stat_activity
WHERE datname = current_database()
  AND wait_event_type = 'Lock'
  AND $1::integer = ANY(pg_blocking_pids(pid));
```

ログには`holder_pid`（阻害元）、`waiting_pid`（待機側）、`wait_event`、実行中のSQLが出る。
行ロックの競合でも、観測時の`wait_event`が`transactionid`になる場合があるため、イベント名を`tuple`に固定しない。
`wait_event_type`と阻害元PIDを合わせて、待機原因を特定する。
根拠：[待機イベントの定義](https://www.postgresql.org/docs/18/monitoring-stats.html#MONITORING-PG-STAT-ACTIVITY-VIEW)、
[pg_blocking_pids](https://www.postgresql.org/docs/18/functions-info.html#FUNCTIONS-INFO-SESSION)。

`hold_after_observation=200ms`は実験で追加した保持時間。
`order_total`はそれを含む注文全体の所要時間で、純粋なロック待ち時間の測定値ではない。
ロック待ちが発生していることは、経過時間からの推測ではなくDBで確認する。

## この区切りで確認すること

- 在庫不足の10件も、注文履歴には残るのはなぜか。
- 同じ商品への注文が20件あっても、在庫確認と更新が同時に進まないのはどこか。
- 注文が遅いという結果だけで、ロック待ちと接続プール待ちを区別できるか。
- ロックを保持したまま処理が長引いたら、後続の注文には何が起こるか。

会話で同時注文と接続プールの理解を確認し、楽観ロックの比較は後回しにする方針となった。
次は[負荷試験とSQL一括化の比較](learning2-load.md)で、②負荷と原因の切り分け・③DBボトルネックの改善を進める。
学習2は同時注文とSQL改善の実施済み範囲で完了した。未実施の楽観ロック比較・過負荷・遅延への対応などは、[後続Issue #10](https://github.com/tokatu4561/ec-microservice/issues/10)へ移し、AWSデプロイ後に扱う。未実施項目を検証済みとする意味ではない。
