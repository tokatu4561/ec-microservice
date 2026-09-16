# 学習2：負荷をかけて原因を調べ、SQL一括化の効果を比較する

[Issue #3](https://github.com/tokatu4561/ec-microservice/issues/3)の負荷試験の単位。
現在の注文処理のボトルネックを観測して改善前後を比べる。
学習2は実施済み範囲で完了し、残りの検証は[AWSデプロイ後のIssue #10](https://github.com/tokatu4561/ec-microservice/issues/10)へ移管した。

## 今回見る改善

変更前のコードには、意図的な遅延や不具合を入れていない。
商品ごとにSQLを発行する実装へ、同じ20商品を注文する負荷を集中させる。
明細数に応じたSQLの発行回数と、商品ロックを保持する時間がどう影響するかを見る。

| 注文処理（20商品・成功時） | 変更前 | 変更後 |
| --- | --- | --- |
| 商品取得と行ロック | 商品ごとに20回 | ID順でまとめて1回 |
| 在庫更新 | 商品ごとに20回 | まとめて1回 |
| 注文ヘッダー保存 | 1回 | 1回 |
| 明細保存 | 商品ごとに20回 | まとめて1回 |
| 計測されるSQL呼び出し（BEGIN・COMMIT込み） | 63回 | 6回 |

変更前は[比較用に保存したコード](../backend/order_baseline_test.go)、
変更後は[実際の注文処理](../backend/orders.go)を実行する。
比較用コードと[測定サーバー](../backend/load_server_test.go)は`_test.go`にあり、通常のAPIバイナリには含まれない。
通常の単品注文・カート注文にも変更後の注文処理が適用される。

ロックを外して速くする変更ではない。商品ID順のロック取得と、在庫・模擬処理・注文保存を同じトランザクションで扱う方針を維持する。
SQL内の`unnest`は同じ位置の配列要素を行に展開する。商品ID・数量・価格などの対応を保って一括更新・保存する。

## 自分で「負荷 → 原因 → 改善の効果」を体験する

負荷生成は[k6のJavaScript](../load/orders.js)。この手順ではPythonを使わない。
Docker Desktopを起動する。ホストへのGo・k6・PostgreSQLのインストールは不要。
コマンドは1ブロックずつ実行し、失敗したら次へ進まず出力を確認する。
同じ実験環境を使うため、別の負荷試験や`load/run.py`とは同時に実行しない。

### 1. 実験環境と測定条件を準備する

ターミナルAで実行する。以降の実行・解析もこのターミナルを使う。

```sh
cd /Users/tokatu/Desktop/ec-microservice
export PATH="/Applications/Docker.app/Contents/Resources/bin:$PATH"
function dc() { docker compose -p ec-learning-load -f compose.load.yaml "$@"; }
export LOAD_SESSION="$PWD/output/load/manual-$(date +%Y%m%d-%H%M%S)"
export ORDER_VARIANT=baseline
export VUS=20 DURATION=30s ITEMS=20 WORKLOAD=shared
export LOAD_RESULTS_DIR="$LOAD_SESSION/$ORDER_VARIANT-vus$VUS"
mkdir -p "$LOAD_RESULTS_DIR"
dc build api
```

`baseline`は改善前、`current`は改善後。まず20人の仮想ユーザーが同じ20商品を注文する条件を比較する。
`VUS`はHTTP処理を繰り返す仮想ユーザー数であり、DB接続数ではない。

**各測定の前に必ず次を実行する。** 専用DBを作り直して在庫・注文履歴・WALの条件を揃える。
通常の`ec-learning`プロジェクトには触れないが、この専用DBの前回データは消える。解析・照合を終えてから次の測定へ進む。

```sh
dc down
dc up -d --wait db
dc exec -T db psql -X -v ON_ERROR_STOP=1 -U ec -d ec_load -f /experiment/seed.sql
dc up -d api
curl --fail --retry 10 --retry-connrefused --retry-delay 1 http://127.0.0.1:18080/metrics
```

応答の`variant`が指定した方式と一致することを確認する。18080番が使用中なら、準備前に`export LOAD_PORT=18081`とし、curlも18081へ変更する。

### 2. k6で負荷をかけ、待っている場所を見る

ターミナルAで実行する。

```sh
dc run --rm --no-deps k6
```

実行中の30秒間に、ターミナルBで次を実行する。事前にコマンドを準備しておくと観測しやすい。

```sh
cd /Users/tokatu/Desktop/ec-microservice
export PATH="/Applications/Docker.app/Contents/Resources/bin:$PATH"
function dc() { docker compose -p ec-learning-load -f compose.load.yaml "$@"; }
curl --fail http://127.0.0.1:18080/metrics
dc exec -T db psql -X -U ec -d ec_load -f /experiment/waits.sql
docker stats --no-stream $(docker ps -q --filter label=com.docker.compose.project=ec-learning-load)
```

必要に応じて負荷中に繰り返す。`pool_acquired=10`なら貸出中10本。
DBの`wait_event_type=Lock`と`blocking_pids`は、接続取得後に別のトランザクションを待っている根拠になる。
APIとDBは別時点の観測なので、件数を引き算して厳密な内訳とはしない。
`docker stats`でAPI・DBだけでなくk6のCPU・メモリも見る。終了間際にはk6コンテナが消えて採取できない場合がある。

**考える質問：接続を10本借りていても、10件の注文が同時に進んでいるとは限らないのはなぜか？**

終了後、ターミナルAに集計表が出る。同じ内容は次で読み直せる。

```sh
cat "$LOAD_RESULTS_DIR/summary.txt"
```

| 表示 | 読み方 |
| --- | --- |
| `requests/s` | HTTP要求数/秒。業務成功数とは区別する |
| `http_req_duration` p95 / p99 | HTTP応答時間の分位点（ms） |
| `system_errors` | 通信失敗・想定外の応答の割合（0～1） |
| `stock_shortage` | 在庫不足の割合（0～1） |
| `checks` | 注文内容の検証成功率。1であること |
| `pool_acquire_ms` | 接続取得時間。新規接続作成も含む場合がある |
| `sql_ms` | SQL呼び出しの累積時間。通信・DB処理・待機を含む |
| `lock_sql_ms` | ロック取得SQLの累積時間。純粋なロック待ち時間ではない |
| `sql_calls` | BEGIN・COMMIT込みのSQL回数/注文。改善前は63回 |

`sql_ms`には`lock_sql_ms`が含まれるので足し合わせない。
しきい値未達では`FAIL`と終了コード99になる。性能のしきい値未達と、`checks`の不一致は区別する。
ファイルが出ない・起動に失敗した場合は測定不成立。前回の結果を流用しない。

### 3. SQLの回数・累積時間を調べる

負荷が終わったら、ターミナルAで実行する。実行計画・整合性照合より先に統計を保存する。

```sh
dc exec -T db psql -X -U ec -d ec_load -f /experiment/queries.sql > "$LOAD_RESULTS_DIR/queries.txt"
cat "$LOAD_RESULTS_DIR/queries.txt"
```

`calls_per_order`はSQL発行回数÷保存された注文数。
商品取得・在庫UPDATE・明細INSERTが各20回になっているかを見る。
失敗して保存されなかった注文がある場合、保存件数で割った値は全リクエストの平均ではない。
`total_ms`は同時実行したSQLの時間も合算され、CPU使用時間とは異なる。
`cache_hits`と`block_reads`はDBのバッファ使用状況であり、後者が必ず物理ディスク読み取りを意味するわけではない。

**考える質問：1回のSQLが速くても、20回繰り返すとトランザクションや他の注文へ何が起きるか？**

### 4. 実行計画を確認する

```sh
dc exec -T db psql -X -v ON_ERROR_STOP=1 -U ec -d ec_load -c \
  'EXPLAIN (ANALYZE, BUFFERS) SELECT id,name,price_yen,stock FROM products WHERE id=1 FOR UPDATE' \
  > "$LOAD_RESULTS_DIR/explain.txt"
cat "$LOAD_RESULTS_DIR/explain.txt"
```

`Index Scan using products_pkey`なら主キーインデックスを使っている。`actual ... rows=1`は取得した行数。
負荷終了後の代表クエリなので、負荷中の待ち時間やアプリのキャッシュ済みprepared statementの計画を直接示すものではない。

**考える質問：インデックスを使って1行だけ取得しているなら、次は何を減らすべきか？**

### 5. 注文と在庫の正しさを確認する

k6はその実行で受け取った成功・在庫不足件数と`checks`判定を`verify.sql`に保存している。
次のコマンドでDBと照合する。SQL本体は[load/verify.sql](../load/verify.sql)で読める。

```sh
dc exec -T db psql -X -U ec -d ec_load < "$LOAD_RESULTS_DIR/verify.sql" > "$LOAD_RESULTS_DIR/integrity.txt"
cat "$LOAD_RESULTS_DIR/integrity.txt"
curl --fail http://127.0.0.1:18080/metrics
```

`passed=t`と`PASS`、不正件数がすべて0であることを確認する。不一致ではpsqlも終了コード3で失敗する。
全商品の「残在庫＋販売数量＝初期在庫」、注文と明細、k6の件数との一致を検証する。
`checks`の失敗・欠落も不合格。タイムアウトを勝手に再送して件数を合わせない。
負荷終了後の`in_flight`と`pool_acquired`が0へ戻ることも確認する。

### 6. 改善後を同条件で測る

変更箇所を読む。

```sh
git diff 1c11b6138126f154e089689dcb06983e14917fb7 -- backend/orders.go
```

商品取得を`ANY(...) ORDER BY id FOR UPDATE`へ、在庫更新と明細保存を`unnest`による一括処理へ変更している。
ロックとトランザクションを維持しながら、SQLの往復を減らす。

ターミナルAで方式と保存先を変更する。

```sh
export ORDER_VARIANT=current
export LOAD_RESULTS_DIR="$LOAD_SESSION/$ORDER_VARIANT-vus$VUS"
mkdir -p "$LOAD_RESULTS_DIR"
```

**手順1の`dc down`から手順5までを再実行する。** 手順1の最初の設定ブロックは再実行しない（baselineへ戻ってしまう）。
手順4のSQLだけは次へ置き換える。

```sh
dc exec -T db psql -X -v ON_ERROR_STOP=1 -U ec -d ec_load -c \
  'EXPLAIN (ANALYZE, BUFFERS) SELECT id,name,price_yen,stock FROM products WHERE id=ANY(ARRAY[1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20]::bigint[]) ORDER BY id FOR UPDATE' \
  > "$LOAD_RESULTS_DIR/explain.txt"
cat "$LOAD_RESULTS_DIR/explain.txt"
```

両方の測定と照合が終わったら比較する。

```sh
cat "$LOAD_SESSION/baseline-vus20/summary.txt"
cat "$LOAD_SESSION/current-vus20/summary.txt"
cat "$LOAD_SESSION/baseline-vus20/integrity.txt"
cat "$LOAD_SESSION/current-vus20/integrity.txt"
```

SQL回数が63→6になったか、処理件数/秒・p95/p99・平均待ち時間がどう変わったかを比べる。
すべて改善するとは限らない。正しさを確認した上で、悪化した指標もそのまま記録する。

さらに1・5・10 VUも試す場合は、`VUS`と`ORDER_VARIANT`、`LOAD_RESULTS_DIR`を変更して、条件ごとにDB再作成から照合まで繰り返す。
再測定で上書きしないよう、新しい`LOAD_SESSION`を作る。
`WORKLOAD=separate`なら各VUが別の商品群を注文する。別条件なので、別のセッションに保存して比較する。

### 7. 後片付け

解析が終わったら専用環境だけを削除する。結果ファイルは残る。

```sh
dc down
```

手動手順の保存ファイルは`summary.txt`、`k6.json`、`verify.sql`、`queries.txt`、`explain.txt`、`integrity.txt`。
観測コマンドの出力は自動保存されない。過去のPythonによる一括実験は`load/run.py`に残しているが、この学習手順には不要。


## 条件を揃える

| 項目 | 標準条件 |
| --- | --- |
| データ | 商品10,000件、各在庫1,000,000、単価500円、注文履歴は各試験前に空 |
| 注文 | 同じ商品ID 1～20、各1個、模擬決済・配送は成功 |
| 同時実行 | k6のconstant-vusで1・5・10・20 VU、各30秒 |
| 上限 | API 2 CPU/512MiB、DB 2 CPU/2GiB、k6 2 CPU/512MiB |
| DB接続 | 注文用プール上限10本、PostgreSQL接続上限40本。観測接続は別枠 |
| 処理期限 | サーバーのcontextは3秒、k6のHTTP要求は5秒 |
| 事前に決めた目安 | HTTP p95 ≤ 500ms、システムエラー率 < 1% |

各VUは1注文の応答を待って次を送る。これは一定の同時実行数による比較であり、一定の到着件数/秒による試験ではない。
改善後は同じVU数でも送信件数が増える。受け付ける量を固定した過負荷・回復試験はIssue #10でAWSデプロイ後に扱う。
専用のウォームアップは設けず、各試験の起動直後から同じ30秒を計測する。初期化・ビルド・整合性照合は計測区間の外。

測定用HTTPから実際の`createOrderTx`を呼ぶ。画面・Next.js・カートCookie・カート編集は通さないので、EC画面全体の応答性能を示す値ではない。
段階ログの大量出力を避け、両実装とも測定中はそのログを抑制する。エラーは記録する。

同じMac上のDocker VMで、負荷生成側とAPI・DBが資源を共有する。
`docker stats`でk6側のCPU・メモリも確認し、負荷生成が制約になっていないか見る。
DB保存先は実験用tmpfsなので、通常の永続ディスクI/Oの性能評価には使わない。

## 正しさも検証する

負荷後に、全商品の「残在庫＋成功注文の販売数量＝初期在庫」、全注文の明細数・単価・数量・商品名を照合する。
さらにk6が受け取った成功・在庫不足件数とDB保存件数を照合する。
タイムアウトで結果不明となった注文を自動再送しない。クライアントとDBの件数が一致しない場合は検証を失敗にする。

```sh
docker compose --profile test run --build --rm api-test
```

このコマンドで通常の全テスト・race検査・整形確認・vet・ビルドを実行する。
新規テストは1・20・100明細の各業務結果を旧実装と比較し、入力の重複・存在しない商品・数量範囲外も確認する。
既存のカート競合、逆順の商品への並行注文、在庫更新後・明細保存途中のDBエラーによるロールバックも実行する。

## 実測例：2026-09-13

標準条件で各ケース1回ずつ測定した結果。実行元はDocker VM 10 CPU・約7.75GiB。
各条件でDBを再作成し、両方式ともDBの上限を2GiBに揃えた。
ローカル成果物は`output/load/20260913-223317/`。再実行では別の数値になる。

| 同時実行 | 変更前 注文/秒 | 変更後 注文/秒 | 変更前 p95 / p99 ms | 変更後 p95 / p99 ms |
| --- | ---: | ---: | ---: | ---: |
| 1 | 214.1 | 334.9 | 6.12 / 7.06 | 4.98 / 5.67 |
| 5 | 215.0 | 338.2 | 32.02 / 36.94 | 43.98 / 108.27 |
| 10 | 212.8 | 337.3 | 63.99 / 67.52 | 99.17 / 169.41 |
| 20 | 212.3 | 332.8 | 161.84 / 226.76 | 139.45 / 211.25 |

全8条件でシステムエラー率0・在庫不足0・k6しきい値判定成功。
計66,038注文を、応答内容・保存した注文と明細・残在庫で照合して成功した。

計測からの判断は以下。

- 変更前は同時実行を増やしても約213～215注文/秒で頭打ちになり、待ち時間が増えた。
- 20同時実行の平均接続取得時間は46.79→29.77ms、ロック取得SQL時間は43.16→26.97ms。
  ロック待機をDBでも観測した。SQL回数を63→6に減らす変更で、スループットは約1.6倍になった。
- ただし5・10同時実行のp95/p99は悪化した。SQL回数削減がすべての応答時間を改善するとは言えない。
  個別の遅いリクエストが生じた詳細原因は、この集計だけでは確定しない。
- 採取したDBメモリ使用量の最大は上限の14.95%、APIは3.06%、k6は5.15%。
  CPU最大はDB約102%、API約38%、k6約11%（Dockerの100%は約1 CPU）。各2 CPU上限には余裕があった。
  約2秒間隔のサンプルであり、瞬間的なピークをすべて捉えたとは扱わない。
- 負荷解除後は全条件で処理中0・プール貸出中0・goroutine 8を確認した。
  接続は切断せず、空き接続としてプールに残って再利用できる。

終了時に削除されたk6などの資源サンプルが一部採取できなかったが、各条件16サンプルを保存し、採取失敗も記録した。
注文メトリクスとDB照合は完了している。

予備測定ではDBコンテナを使い回し、メモリが1GiB上限近くまで増加した。
その結果は条件の持ち越しがあるため、上記の最終比較には使っていない。
計測条件を揃えること自体も、性能改善の検証に必要だった。

追加の切り分けとして、各VUが別の20商品を注文する条件も20 VU・30秒で1回ずつ実行した。
当時はPythonの一括実行で計測した。現在の手順では`VUS=20 WORKLOAD=separate`に相当する。成果物は`output/load/20260913-223900/`。

| 別商品群・20 VU | 変更前 | 変更後 |
| --- | ---: | ---: |
| 注文/秒 | 308.9 | 340.7 |
| p95 ms | 105.66 | 106.14 |
| p99 ms | 173.41 | 184.17 |
| 平均ロック取得SQL ms | 5.28 | 0.67 |

こちらもエラー0・在庫不足0・応答とDBの照合成功。
両方式とも採取した16サンプルでDBのロック待ちは0件だった。一方、DBのCPUは約200%と2 CPUの割り当て近くに達した。
同じ商品への集中時と異なり、この条件ではDBのCPUも制約となっている可能性がある。
商品への集中をなくすとロック取得SQL時間は減るが、p95/p99の改善は確認できなかった。
したがって、遅い側のリクエストの原因を同一商品のロック競合だけで説明することもできない。
今回確認できた改善はSQL呼び出し回数・スループット・平均時間の改善であり、裾のレイテンシを一律に改善したとは扱わない。

## 今回の範囲

SQL実行計画でインデックス利用を確認し、SQLの個別発行をまとめる改善を扱う。
新しいインデックスの追加、楽観ロック、受付制限、下流の遅延注入とキャンセル検証は別の単位。
利用者の判断によりIssue #3はこの実施済み範囲で完了した。未実施の楽観ロック比較・受付制限・下流の遅延とキャンセル検証などはIssue #10へ引き継ぎ、AWSデプロイ後に行う。
設計判断と比較結果の扱いは[ADR 0005](adr/0005-bulk-order-sql-load-comparison.md)に記録した（状態は提案）。

参考：[k6のconstant-vus](https://grafana.com/docs/k6/latest/using-k6/scenarios/executors/constant-vus/)、
[PostgreSQLのSQL統計](https://www.postgresql.org/docs/18/pgstatstatements.html)、
[pgxpoolの取得時トレース](https://pkg.go.dev/github.com/jackc/pgx/v5/pgxpool#AcquireTracer)。
