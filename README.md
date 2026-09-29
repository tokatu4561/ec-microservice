# ECのマイクロサービス学習プロジェクト

注文・在庫・決済・配送基盤を題材に、設計判断と障害検証を学ぶプロジェクトです。
全体像は[構想書](ec_microservices_learning_project.md)を参照してください。

## AI駆動開発

**Codexで計画 → 人が承認 → 実装・検証 → reviewerへ委任 → 必要な修正・再レビュー → ADR → 人の最終確認**

修正は最大2回とモデルに指示します。機械的な回数制限・時間制限は設けません。
独自の制御プログラムや専用テスト基盤は使いません。

運用ルールは[AGENTS.md](AGENTS.md)に集約しています。
要件はGitHub Issueで管理し、[create-issueスキル](.agents/skills/create-issue/SKILL.md)で最小のIssueを作成します。
例えば「`$create-issue 注文キャンセル機能のIssueを作成して`」と依頼します。
本文は目的・受入条件が基本で、対象外・制約・未決事項・関連資料は必要な場合だけ加えます。
細かな実装検討は会話、必要な引継ぎはIssueコメント、実際の変更と検証結果はPRにまとめます。
コードに現れない設計判断のwhyは[ADR](docs/adr/)に残します。

[学習2 #3](https://github.com/tokatu4561/ec-microservice/issues/3)は、実施済みの範囲で完了しました。
[同時注文と悲観ロックの実験](docs/learning2-concurrency.md)では、在庫10への同時注文20件の整合性と、DBで観測するロック待ちを扱います。
[負荷試験とSQL一括化の比較](docs/learning2-load.md)では、k6で変更前後の処理件数・待ち時間と在庫の正しさを確認しました。
残りの競合制御・過負荷・キャンセル検証は[AWSデプロイ後の検証 #10](https://github.com/tokatu4561/ec-microservice/issues/10)へ引き継いでいます。
[学習3：注文フローのサービス分割 #4](https://github.com/tokatu4561/ec-microservice/issues/4)は、実装・障害検証・振り返りを終え、利用者が学習完了を確認しました。
次は[学習4：冪等性・Outbox・Saga #5](https://github.com/tokatu4561/ec-microservice/issues/5)で、非同期連携と自動復旧を扱います。
最初の単位は[Paymentを独立して動かす](docs/learning3-payment.md)です。
専用DBを持つ模擬決済サービスの作成・照会・取消、再起動と単独停止を扱います。
[同期HTTP連携](docs/learning3-order-payment.md)に続き、[短いトランザクションと注文の手動復旧](docs/learning3-order-recovery.md)まで実装しています。
現在は通信前に注文と進行段階を保存し、通信中のDBロックを解放します。[OpenTelemetryとJaegerのデモ](docs/learning3-tracing.md)で、サービス間の時間・障害・再開を可視化できます。通常環境のJaegerは http://localhost:16686 です。

Shippingも専用DBを持つ別サービスへ分離しました。[Shippingの設計・障害復旧デモ](docs/learning3-shipping.md)で、決済成功後の配送結果不明と手動再開を確認できます。Inventoryも別サービスへ分離済みです。[現行の4サービス構成](docs/learning3-inventory.md)を参照してください。

実装・検証後は、親エージェントが[レビュー担当](.codex/agents/reviewer.toml)を起動し、結果を受け取ります。
毎回のレビュー依頼や、指摘の手動コピーは原則不要です。修正・再検証は親が担当します。
親がIssueの要件と検証結果をreviewerに渡すため、ローカルの計画ファイルは不要です。
reviewerは編集せず、テスト不足も確認します。テスト作成・実行専用のエージェントは現在設けません。
サブエージェントを使えるCodex環境が前提で、利用できない場合や修正上限に達した場合は人に報告します。

## 学習1：ローカルECの注文フロー

[Issue #2](https://github.com/tokatu4561/ec-microservice/issues/2)の実装です。
学習1ではGoの単一バックエンドとPostgreSQLで、商品一覧・注文・在庫確保・模擬決済・模擬配送・注文状況を実装しました。
現在は学習3の変更で決済・配送・在庫が専用サービス・専用DBに分かれています。
Next.jsは画面とAPI転送を担当します。実際の支払いや配送は発生しません。

### 起動・停止

DockerエンジンとComposeが必要です。Go・Nodeのホストへのインストールは不要です。
このMacで`docker`が見つからない場合は、Docker Desktopを起動し、以下を実行します。

```sh
export PATH="/Applications/Docker.app/Contents/Resources/bin:$PATH"
docker --version
docker compose version
docker info
```

リポジトリのルートで実行します。初回は公式イメージとパッケージを取得します。
既存環境を更新する場合は先に `docker compose stop api web` で旧Orderを停止してください。
在庫のオフライン移行を行うため、新旧Orderの混在運用はできません。

```sh
docker compose up --build --wait --wait-timeout 120
```

[商品一覧](http://localhost:3000)を開きます。3000番ポートを使用中なら
`WEB_PORT=3001 docker compose up --build --wait`として3001番を開きます。
画面を127.0.0.1へ公開し、既存Go・DBにはCompose内部から接続します。
学習3のPayment APIは別途127.0.0.1:8081へ公開します（`PAYMENT_PORT`で変更可能）。固定のDB認証情報はローカル学習用です。

```sh
docker compose down
docker compose up --wait
```

`down`でコンテナを削除しても、名前付きボリュームに商品・カート・注文が残ります。
初期SQLは空のDBを最初に起動したときに実行されます。既存の商品データを保ったまま、
`migrate`サービスが注文テーブルの追加SQLを実行し、成功してからAPIを起動します。
追加SQLは再実行可能です。003で既存注文を1明細の注文へ移し、ID・日時・状態・当時の名称と価格・在庫を保持します。

以下は旧教材の商品初期投入SQLです。Inventory移行後の在庫補充には使えません。商品追加・補充APIは未実装です。

```sh
docker compose exec -T db psql -v ON_ERROR_STOP=1 -U ec -d ec < db/init/001_products.sql
```

**学習用の商品変更と注文履歴をすべて消して初期化したい場合だけ**、次を実行します。

```sh
docker compose --profile test down --volumes
docker compose up --wait
```

### 画面で確認する

商品一覧で数量を入力して「カートに追加」を押します。同じ商品は数量を加算します。
カートで数量変更・削除を行い、模擬決済・配送のモードを選んで一括注文します。
1つの注文IDに複数明細を保存し、成功時だけカートを空にします。失敗時は全明細を残します。
送信中は編集と再送を無効化し、通信エラーでは自動再送しません。
カートのバージョンと行ロックで同じカートの競合を409にします。409後は最新内容を確認して操作し直してください。

| 試すこと | 注文の結果 | 在庫 | 模擬決済 | 模擬配送 |
| --- | --- | --- | --- | --- |
| 在庫のある商品、両方成功 | 配送依頼済み | 注文数量だけ減る | 成功 | 依頼済み |
| 在庫を超える数量（または在庫0の商品） | 失敗・在庫不足 | 変化なし | 未実施 | 未実施 |
| 決済を失敗にする | 失敗・決済失敗 | 確保分を戻す | 失敗 | 未実施 |
| 決済成功、配送を失敗にする | 失敗・配送失敗 | 確保分を戻す | 取消済み | 失敗 |

結果の注文IDを保存すると、「注文状況を確認する」や`/orders/注文ID`から後で確認できます。
名称と単価は注文時点の値を保存するため、商品データを変更しても過去の注文表示は変わりません。
価格は整数の円、数量は1～2147483647の整数です。入力不正・存在しない商品では注文を保存しません。

### GoとDBの処理を追う

```text
ブラウザー POST /api/cart/checkout
  → Next.jsが /api をGoへ転送
  → Go: JSON検証・注文ID発行
  → BEGIN
  → カート行を FOR UPDATE、バージョンを確認
  → 対象商品を商品ID昇順で SELECT ... FOR UPDATE
  → 注文・明細・inventory_pendingを保存し、カートを処理中にする
  → COMMIT（DB接続・ロックを解放）
  → Inventoryを照会・予約（HTTP）。在庫不足なら決済を呼ばず失敗
  → 段階を保存し、Paymentを照会・決済（HTTP）
  → Shippingの配送依頼を照会・作成（HTTP）
  → 明確な配送受付失敗なら取消待ちを保存して、Paymentの取消APIを呼ぶ
  → Inventoryの予約を確定／解除（HTTP）
  → 短い別トランザクションで注文・カートを確定
  → 結果不明なら予約を保持し、同じ注文から手動で再開
```

HTTPの入口は`backend/http.go`、受付は`backend/cart.go`の`Checkout`と`backend/orders.go`の`CreateOrder`です。
`backend/order_progress.go`が在庫予約、Payment／Shipping通信、確定を分離します。
通信中にはDBトランザクションを保持しません。在庫不足ではPaymentを呼ばず失敗を保存します。
予約の結果不明では注文を保持し、Paymentを呼ばずに再開を待ちます。
保存後の通信・確定失敗では注文と予約が残るため、注文詳細の「処理を再開」から続行します。
取消待ちも永続化し、Inventoryは予約行ロックで数量を一度だけ戻し、Orderは段階の比較更新と注文行ロックで進行・カートを確定します。
実際の課金・返金・配送は行いません。自動復旧、予約の自動期限切れ、旧実験で作った孤立決済の移行は対象外です。

```sh
curl -i http://localhost:3000/api/products
curl -i http://localhost:3000/api/orders \
  -H 'Content-Type: application/json' \
  -d '{"productId":1,"quantity":1,"paymentMode":"success","shippingMode":"fail"}'
# 応答のidを下記の注文ID部分へコピーする
curl -i http://localhost:3000/api/orders/注文ID

docker compose logs --tail 50 api
docker compose exec inventory-db psql -U inventory -d inventory -c 'SELECT product_id, available FROM stocks ORDER BY product_id;'
docker compose exec db psql -U ec -d ec -c 'SELECT id, status, failure_reason, payment_status, shipping_status FROM orders ORDER BY created_at;'
```

`X-Request-ID`をログの`request_id`と照合し、`order_id`で注文処理を追います。
`inventory_call_finished`・`payment_call_finished`・`shipping_call_finished`で外部呼び出しの結果を追います。
`order_finalized`は注文確定後の記録です。通信失敗では相手DBの未保存とは断定せず、同じ注文IDで照会します。
定期的な商品取得ログはComposeのヘルスチェックでも発生します。

### APIの契約

| API | 成功応答 | 主なエラー |
| --- | --- | --- |
| `GET /api/products` | 200、`{products: [...]}` | DB障害503 |
| `POST /api/orders` | 201、`{order: {...}}`とLocationヘッダー | 入力400、形式415、商品不在404、DB障害503 |
| `GET /api/orders/{id}` | 200、`{order: {...}}` | ID形式400、注文不在404、DB障害503 |

POSTは`productId`・`quantity`を必須とし、`paymentMode`・`shippingMode`に`success`または`fail`を受け取ります。
模擬モードの省略時は成功です。未知のJSONフィールド・複数JSON・4096バイトを超える本文を拒否します。
失敗履歴も保存されるため、業務上の失敗でもHTTPは201です。`order.status`と`failureReason`で業務結果を判断します。
注文状態は`processing`（確認待ち）・`cancel_pending`（決済取消待ち）・`shipping_requested`（配送依頼済み）・`failed`（失敗）、失敗理由は`out_of_stock`・`payment_failed`・`shipping_failed`です。

### 検証を再実行する

```sh
# 独立した一時DBでGoの単体・DB統合・race・整形確認・vet・ビルド
docker compose --profile test run --build --rm api-test
# TypeScript型検査・ESLint・本番ビルド
docker compose build web
# 起動中のアプリへHTTP注文を作成（成功1件で2商品の在庫が1ずつ減る）
python3 scripts/smoke.py
# テストDBを停止（一時データは破棄される）
docker compose --profile test stop test-db
```

PythonはHTTPスモークテストだけに使います。Go・Nodeの検証はコンテナ内で実行します。
Goテストは正常・在庫不足・決済失敗・配送失敗、入力検証、注文時点の価格保持を確認します。
在庫1への並行2注文では成功1件・失敗1件・在庫0を検証します。
テスト専用DBのトリガーで注文保存失敗や予約後の進行更新失敗を再現し、外部処理前のロールバックと、外部処理後の安全な再開を確認します。
通常の`go test`では`TEST_DATABASE_URL`未設定ならDB統合テストをスキップするため、上記Composeコマンドを使ってください。

GitHub Actionsにも同じ検査とHTTP注文フローを定義しています。push未実施の間はリモートでの成功を未確認として扱います。
ESLint 9.39.5のサポート終了警告が残っています。現在の検査結果と依存更新の必要性は分けて判断します。

DB停止時のエラーを試すには`docker compose stop db`後に画面を再読み込みします。
検証後は`docker compose start db`で戻し、DBが起動してから再読み込みしてください。

設計判断は[ADR一覧](docs/adr/)を参照してください。ADRの採用と学習内容の最終確認は学習者が行います。

### DB保存の匿名カートと一括注文

[Issue #8](https://github.com/tokatu4561/ec-microservice/issues/8)の追加機能です。[ER図とテーブルの役割](docs/data-model.md)を参照してください。
カートは購入予定、注文は確定した処理履歴です。カートの価格・在庫は現在の商品値を表示し、注文時にGoがDBの価格を取得して明細へ保存します。
カートへの追加では在庫を確保しません。1商品でも不足すれば注文全体を失敗にし、全商品の在庫を維持します。

匿名トークンはHttpOnly・SameSite=LaxのCookie（Path=/api/cart）に保存し、DBにはSHA-256ハッシュだけを保存します。
期限は作成から30日で、期限切れ後のGETは新しい空カートを作ります。期限切れデータの定期削除はありません。
ローカルHTTPで利用する構成です。別ブラウザーとは共有せず、認証も追加していません。

| API | 本文 | 成功応答 |
| --- | --- | --- |
| GET /api/cart | なし | 200 `{cart}`。初回Cookie発行 |
| PUT /api/cart/items/{productId} | `{version, quantity}`（絶対数量） | 200 `{cart}` |
| DELETE /api/cart/items/{productId} | `{version}` | 200 `{cart}` |
| POST /api/cart/checkout | `{version, paymentMode, shippingMode}` | 201 `{order}` |

変更リクエストは`Content-Type: application/json`と`X-Cart-Request: 1`が必要です。
バージョンが古い場合は409、空カート・不正入力は400、商品不在・期限切れカートは404、DBエラーは503です。
カートは最大100商品、数量は正の32ビット整数、円の合計はJavaScriptが正確に表現できる整数範囲内に制限します。
旧`POST /api/orders`の単品入力も利用できます。注文応答は共通で`items`配列と`totalYen`になり、旧トップレベルの商品名・数量・価格は廃止しました。
各明細は`productId, productName, quantity, priceYen, subtotalYen, stockShortage`を持ちます。

カート行を先にロックし、商品行を常にID昇順でロックすることで、逆順の商品を注文しても循環待ちを防ぎます。
Orderの短いトランザクションで商品情報と注文を保存します。在庫予約は別HTTP要求とInventory側のトランザクションで行います。
カートの版を受付時に1つ進め、`pendingOrderId`と`lastOrderId`へ注文IDを保存します。
処理中のカートは編集・再注文とも409になります。確定後、成功時は内容を消去し、失敗時は内容を残します。
処理中のカートは期限を過ぎても既存Cookieで照会できます。ただしCookie自体の期限後の再発行・所有者復旧機能はありません。
`POST /api/orders/{id}/resume`は本文`{}`と上記2ヘッダーを指定します。確定済みなら結果を返すだけです。
復旧は同じ注文IDを使用します。別の新規注文要求に対する汎用的な冪等性キーは未実装です。

DB統合テストには、逆順の複数商品並行注文、同じカートへの競合更新と注文、全商品更新後と明細保存途中のDBエラー、
匿名Cookieの分離・期限切れ、旧注文の移行・再実行・空DB構築も含みます。CIも同じComposeテストと更新したスモークテストを実行します。

### 学習3：Inventoryの分離

現在はOrder・Payment・Shipping・Inventoryの4サービス。Inventoryが販売可能数と予約を所有します。
Orderは処理段階を永続化し、通信断後は同じ注文の「処理を再開」で復旧します。
在庫照会不能は画面に「在庫確認不可」と表示します。
[設計図・移行の注意・停止復旧と4サービスのトレースのデモ](docs/learning3-inventory.md)を参照してください。
既存環境の更新前には旧Orderを停止してください。`inventory-import`が在庫と保留予約を一度だけ移行します。
