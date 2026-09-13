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

実装・検証後は、親エージェントが[レビュー担当](.codex/agents/reviewer.toml)を起動し、結果を受け取ります。
毎回のレビュー依頼や、指摘の手動コピーは原則不要です。修正・再検証は親が担当します。
親がIssueの要件と検証結果をreviewerに渡すため、ローカルの計画ファイルは不要です。
reviewerは編集せず、テスト不足も確認します。テスト作成・実行専用のエージェントは現在設けません。
サブエージェントを使えるCodex環境が前提で、利用できない場合や修正上限に達した場合は人に報告します。

## 学習1：ローカルECの注文フロー

[Issue #2](https://github.com/tokatu4561/ec-microservice/issues/2)の実装です。
Goの単一バックエンドとPostgreSQLで、商品一覧・注文・在庫確保・模擬決済・模擬配送・注文状況を扱います。
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

```sh
docker compose up --build --wait --wait-timeout 120
```

[商品一覧](http://localhost:3000)を開きます。3000番ポートを使用中なら
`WEB_PORT=3001 docker compose up --build --wait`として3001番を開きます。
画面だけを127.0.0.1へ公開し、Go・DBにはCompose内部から接続します。固定のDB認証情報はローカル学習用です。

```sh
docker compose down
docker compose up --wait
```

`down`でコンテナを削除しても、名前付きボリュームに商品・カート・注文が残ります。
初期SQLは空のDBを最初に起動したときに実行されます。既存の商品データを保ったまま、
`migrate`サービスが注文テーブルの追加SQLを実行し、成功してからAPIを起動します。
追加SQLは再実行可能です。003で既存注文を1明細の注文へ移し、ID・日時・状態・当時の名称と価格・在庫を保持します。

初期商品を再投入する場合は以下です。既存IDの価格・在庫・注文は変更しません。

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
  → 在庫不足なら失敗状態を決定
  → 在庫があれば確保 → 模擬決済 → 模擬配送
  → 模擬失敗なら在庫を戻す
  → ordersに全体状態、order_itemsに全明細をINSERT
  → 成功時にカートを空にし、成功・業務失敗ともバージョンを更新
  → COMMIT → JSONで注文を返す → 画面で表示
```

`backend/http.go`の`newHandler`がHTTPの入口、`backend/cart.go`の`Checkout`と`backend/orders.go`の`createOrderTx`が一括注文処理です。
`pgx.Tx`内のSQLはすべて同じDBトランザクションに属し、途中でDBエラーが出るとロールバックします。
`defer`で後始末を予約し、リクエストがキャンセルされても別の短い猶予でロールバックを試みます。

業務上の失敗（在庫不足・模擬失敗）は注文履歴としてコミットします。
DBの途中エラーでは在庫と注文の部分更新を残しません。
ただしCOMMIT応答やHTTP応答が失われた場合、呼出側には保存済みか分からないことがあります。
この場合は自動再送せず、返された注文IDで照会するか、リクエストID・注文IDとDBを照合してください。
この学習では実外部サービスを呼ばないため、模擬決済取消も同一DB内の状態変更です。実決済の返金をDBロールバックだけで解決できるという意味ではありません。

```sh
curl -i http://localhost:3000/api/products
curl -i http://localhost:3000/api/orders \
  -H 'Content-Type: application/json' \
  -d '{"productId":1,"quantity":1,"paymentMode":"success","shippingMode":"fail"}'
# 応答のidを下記の注文ID部分へコピーする
curl -i http://localhost:3000/api/orders/注文ID

docker compose logs --tail 50 api
docker compose exec db psql -U ec -d ec -c 'SELECT id, name, price_yen, stock FROM products ORDER BY id;'
docker compose exec db psql -U ec -d ec -c 'SELECT id, status, failure_reason, payment_status, shipping_status FROM orders ORDER BY created_at;'
```

`X-Request-ID`をログの`request_id`と照合し、`order_id`で注文処理を追います。
`stock_reserved_in_transaction`や`stock_restored_in_transaction`はまだ確定前の操作です。
単品の`order_committed`、カートの`order_and_cart_committed`がコミット応答を受け取った記録です。`order_not_committed_by_application`はアプリがコミット成功を確認できなかった記録であり、通信障害時のDB上の未保存を断定するものではありません。
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
注文状態は`shipping_requested`または`failed`、失敗理由は`out_of_stock`・`payment_failed`・`shipping_failed`です。

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
テスト専用DBのトリガーで在庫更新後の注文INSERTを失敗させ、在庫が元に戻り注文が残らないことも確認します。
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
Read Committedの同一トランザクションで全商品の確認・在庫更新・模擬処理・注文保存・カート消去を行います。
DB途中エラーではすべてロールバックします。業務失敗では失敗注文をコミットし、カートを維持してバージョンを進めます。
`cart.lastOrderId`で直近の注文を確認できます。汎用的な冪等性キーや実決済は対象外です。

DB統合テストには、逆順の複数商品並行注文、同じカートへの競合更新と注文、全商品更新後と明細保存途中のDBエラー、
匿名Cookieの分離・期限切れ、旧注文の移行・再実行・空DB構築も含みます。CIも同じComposeテストと更新したスモークテストを実行します。
