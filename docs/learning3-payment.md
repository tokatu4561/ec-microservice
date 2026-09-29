# 学習3・第1単位：Paymentを独立して動かす

[Issue #4](https://github.com/tokatu4561/ec-microservice/issues/4)の最初の単位です。
今回の到達点は、決済機能を別プロセス・別DBで起動し、作成・照会・取消をAPIで扱えることです。
以下の構成図と責務説明は第1単位時点のものです。**現在は[第2単位の同期HTTP連携](learning3-order-payment.md)まで進んでおり、注文はPaymentを呼びます。**
現在の注文フローは[第3単位](learning3-order-recovery.md)を参照してください。Payment単体APIは下記の再送契約へ更新しています。

## なぜ分割するのか

実務で分割を検討する理由は、決済手段の追加や障害修正を、注文・在庫とは別の単位で
リリース・停止・復旧できるようにすることです。決済記録の更新ルールと責任も一箇所へ集めます。
このリポジトリで、複数チームの調整や性能の問題が実際に確認されたわけではありません。
今は、独立性の利点と、通信・複数DBによって増える負担を比較するための学習です。

| 責務 | 管理すること | 今回の配置 |
| --- | --- | --- |
| Order | 注文全体の進行、明細、注文結果 | 既存Goアプリ |
| Inventory | 在庫の確保・解放 | 既存Goアプリ |
| Payment | 模擬決済の成功・失敗・取消記録 | 新しいGoサービス。ただし注文への接続は次単位 |
| Shipping | 配送依頼とその結果 | 既存Goアプリ |

```text
ブラウザー → Next.js → 既存Goアプリ → 既存DB
                         注文・在庫・従来の模擬決済・模擬配送

curl → Payment :8081 → Payment専用DB
       決済・照会・取消   paymentsテーブル
```

単一アプリ内でもモジュール分離はできます。今回は独立して起動・停止する境界を体験するため、
プロセスとDBも分けました。Inventory先行は商品・カートへの影響が大きいため、Paymentを先にします。
通信は追跡しやすい同期HTTPから始め、キュー・非同期イベントは追加しません。

## 起動してAPIを操作する

リポジトリのルートで実行します。Dockerが見つからない場合はREADMEのPATH設定を参照してください。

```sh
# Paymentと専用DBだけをビルド・起動する
# 初回起動時にPaymentが埋め込みSQLでテーブルを作成する（再実行可能）
docker compose up --build --wait --wait-timeout 120 payment
curl -i http://127.0.0.1:8081/healthz

# 実験ごとに新しい注文IDを用意する。既存注文の存在確認は行わない
ORDER_ID=$(python3 -c 'import uuid; print(uuid.uuid4().hex)')

# 1. 模擬決済を保存する。X-Request-IDは応答とログに引き継がれる
curl -i http://127.0.0.1:8081/payments \
  -H 'Content-Type: application/json' \
  -H 'X-Request-ID: lesson3-create' \
  -d "{\"orderId\":\"$ORDER_ID\",\"amountYen\":980}"

# 2. 保存結果を照会する
curl -i "http://127.0.0.1:8081/payments/$ORDER_ID" \
  -H 'X-Request-ID: lesson3-lookup'

# 3. 取り消す。本文は付けない。2回実行しても取消済みの記録を返す
curl -i -X POST "http://127.0.0.1:8081/payments/$ORDER_ID/cancel" \
  -H 'X-Request-ID: lesson3-cancel'

# 4. APIの結果とDB・ログを照合する
docker compose exec -T payment-db psql -U payment -d payment \
  -c 'SELECT order_id, amount_yen, status, created_at, updated_at FROM payments ORDER BY created_at;'
docker compose logs --tail 30 payment
```

作成は201と `status: succeeded`、照会は200、取消は200と `status: cancelled` になります。
`mode: fail` を指定した別IDの決済は201と `status: failed` を返します。
201は「決済処理の記録を作った」という意味で、業務上の成功は `payment.status` で判断します。
実際の課金・返金は発生しません。

8081番が使用中なら `PAYMENT_PORT=8083 docker compose up --build --wait payment` とし、
curlのポートも変更してください。環境変数はその後のCompose操作でも同じ値を使います。

## 契約と状態の読み方

| API | 入力 | 結果 |
| --- | --- | --- |
| `POST /payments` | `orderId, amountYen, mode` | 201 `{payment}`、同じID・金額・モードは既存結果、異なる内容は409 |
| `GET /payments/{orderId}` | なし | 200 `{payment}`、未登録は404 |
| `POST /payments/{orderId}/cancel` | 本文なし | 成功済み・取消済みは200、失敗済みは409、未登録は404 |
| `GET /healthz` | なし | 専用DBへ接続可能なら200、利用不可は503 |

決済応答は `orderId, amountYen, status, createdAt, updatedAt` を持ちます。
注文IDは32桁の小文字16進文字列、金額は0〜9007199254740991の整数円です。
金額の省略・nullは拒否します。`mode` は `success` または `fail`、省略・空文字は `success` です。
入力不正は400、作成APIのJSON以外のContent-Typeは415です。
未知フィールド・複数JSON・4096バイトを超える本文も400にします。

```text
新規決済 → succeeded → cancelled
         → failed       （failedから取消はできない）
```

注文IDはPayment側の主キーで、別DBの注文テーブルへの外部キーは持ちません。
同じIDで金額やモードを変えて作成しても409となり、記録は上書きしません。
これは最小限の重複拒否で、同じ要求に同じ成功応答を返す汎用的な冪等性APIではありません。
取消は行ロックで直列化し、再取消では日時も変更しません。

`X-Request-ID` は英数字・ピリオド・ハイフン・アンダースコアの1〜128文字を受け取り、
未指定・範囲外なら新しく生成します。ログの `service, request_id, order_id, result` を追ってください。
現時点ではPayment内の相関ログです。サービスをまたぐ追跡はOrderとの接続時に確認します。

DBエラーは503を返しますが、書き込み応答が失われた場合、保存済みの可能性があります。
自動再送はせず、同じ注文IDをGETで照会してください。
今回のサービス内にも通信結果の曖昧さはあり、独立サービス化だけでは解決しません。

## 独立性と永続性を確認する

```sh
# Paymentだけを再起動して同じ記録が返ることを確認する
docker compose restart payment
curl "http://127.0.0.1:8081/payments/$ORDER_ID"

# Payment停止中も商品一覧は読めるが、第2単位以降の新しい購入は503になる
# 既存アプリが未起動なら先に docker compose up --build --wait web を実行する
docker compose stop payment
# http://localhost:3000 から商品・カート・注文を操作する

# Paymentを戻す。記録は専用の名前付きボリュームに残る
docker compose up -d --wait payment
curl "http://127.0.0.1:8081/payments/$ORDER_ID"
```

第1単位では注文がPaymentに依存せず、Payment停止中も注文できました。
現在は接続済みのため、新しい購入は503になります。この変化が実行時の依存関係です。
DBはPayment専用ネットワークに置き、ホストへDBポートを公開しません。
認証情報はローカル学習用で、Payment APIには認証を付けず127.0.0.1だけへ公開しています。

## 自動検証

```sh
# テスト専用の一時DB。通常データは使わない
docker compose --profile test run --build --rm payment-test
# 既存の注文・在庫・カートの回帰
docker compose --profile test run --build --rm api-test

# 起動中のPaymentへ模擬決済の記録を追加して確認
python3 scripts/payment_smoke.py
# Payment・専用DBを再起動して永続性を確認する（稼働中の注文がない検証環境で実行）
python3 scripts/payment_smoke.py --lifecycle
```

Go検証はHTTP契約、DB保存、失敗済み決済の取消拒否、同じ要求の20件同時作成で全件成功・保存は1件、
20件同時取消で同じ結果となること、DBエラー、初期SQL再実行を確認し、race・vet・ビルドも行います。
`TEST_DATABASE_URL`なしのGoテストはDB統合をスキップするため、上記Composeコマンドを使ってください。

既存の学習データを変えずに実HTTP検証する場合は、別Composeプロジェクトを使います。
以下の環境変数はその検証用ターミナル内だけで設定してください。

```sh
export COMPOSE_PROJECT_NAME=ec-learning3-verify WEB_PORT=3002 PAYMENT_PORT=8082
export BASE_URL=http://127.0.0.1:3002 PAYMENT_BASE_URL=http://127.0.0.1:8082
docker compose up --build --wait --wait-timeout 120
python3 scripts/payment_smoke.py --lifecycle
# この検証用プロジェクトだけを停止。ボリュームは保持する
docker compose --profile test down
```

## ここで確認すること

- Paymentだけのコード・DBを変更しても、既存の注文アプリを再ビルドする必要はあるか。
- 同じ注文IDを使うことと、DBのトランザクションを共有することは何が違うか。
- Paymentで成功した後にOrderで失敗すると、どちらのDBの変更が戻るか。

[第2単位](learning3-order-payment.md)では注文からPaymentを同期HTTPで呼びます。Paymentの成功後に注文が失敗するケース、
成功応答の消失、取消通信の失敗などを観察する計画を改めて確認します。
Saga・Outbox・自動再試行の完成は今回の対象外です。
