# 学習3・第4単位：分散トレーシングをデモで理解する

> この文書はInventory分離前の学習段階を説明します。現在の構成・移行・復旧手順は[Inventory分離](learning3-inventory.md)を参照してください。

Issue: https://github.com/tokatu4561/ec-microservice/issues/4

## このデモで分かること

ログを注文IDで集めるだけでは、並行した処理の親子関係や待ち時間を組み立てる作業が必要です。
トレースは1回の要求を処理の木とタイムラインで示します。以下を順に比較します。

1. 正常：OrderとPaymentが1つの処理の木としてつながる。
2. 遅延：時間を使った場所がOrderの在庫処理か、PaymentのDB操作か分かる。
3. タイムアウト：途中で処理が終わり、注文確定に進まなかったことが分かる。
4. 手動再開：同じ注文の別の実行を、別トレースとして追える。
5. 配送失敗：決済作成の後に取消が呼ばれる補償の経路を確認できる。

## 1. 専用環境を起動する

リポジトリのルートで実行します。通常の3000番環境とデータを分けます。

```sh
WEB_PORT=3002 PAYMENT_PORT=8082 JAEGER_PORT=16687 \
  docker compose -p ec-tracing-demo up --build --wait --wait-timeout 120
```

- アプリ：http://localhost:3002
- Jaeger：http://localhost:16687
- Payment：http://localhost:8082

JaegerはOTLP/HTTPをCompose内の4318番で受信します。ホストへ公開するのは127.0.0.1のUIだけです。
Jaegerはこの構成ではメモリ保存です。Jaegerを再起動・撤去するとトレースは消えます。注文DBは別の永続ボリュームです。
初回の画面起動には少し時間がかかることがあります。

## 2. 5つのケースを実行する

```sh
python3 scripts/tracing_demo.py --project ec-tracing-demo
```

Python標準ライブラリとDocker CLIだけで実行できます。各ケースの注文ID・トレースID・Jaegerリンクを表示します。
スクリプトはトレースの親子関係、エラーspan、再開時の別trace、Order/Paymentログのtrace_id/span_idも自動照合します。
同じ商品の販売可能在庫が4個以上必要で、正常・遅延・復旧で合計3個消費します。配送失敗の1個は予約解除します。
繰り返して在庫がなくなった場合は別の在庫のある商品を自動選択します。全商品不足なら終了します。

遅延実験は検証専用Payment DBのpaymentsテーブルを短時間ロックします。通常環境へこのスクリプトを向けないでください。
0.6秒のロックで遅延、1.6秒のロックで1秒のHTTPタイムアウトを再現します。
異常終了時にも接続の終了とDBの5秒のアイドルTX期限によりロックが残り続けないようにしています。
通常APIに故障用のパラメーター・エンドポイントは追加していません。

## 3. 正常注文を読む

出力された「1 正常注文」のリンクを開きます。上部のServicesは2です。
左側にサービス名・処理名、右側に時間の棒が表示されます。

```text
order: POST /api/orders
  order.accept
    order.reserve
    order.resume
      payment.get                 Order側HTTPクライアント
        payment: GET /payments/{orderId}
          payment.store.get       Payment側DB操作
      payment.create
        payment: POST /payments
          payment.store.create
      order.finalize
```

最初の照会が404でも、新規注文では正常です。まだ決済がないので次にPOSTします。この404を障害spanにはしません。
「HTTPエラー」と「決済の業務失敗」「未登録」を同じものとして読まないことがポイントです。

棒をクリックして展開すると、order_idやHTTPステータスなどのタグを確認できます。
上部のDurationが要求全体です。親の棒には子の時間も含まれるので、全spanの時間を足してはいけません。
ブラウザーの描画やNext.jsの転送は今回の計装範囲外です。Goが要求を受けた時点から記録しています。

## 4. 遅延注文を正常と比較する

「2 PaymentのDB待ち」を開き、`payment.store.get`を探します。
実測例（2026-09-24、専用ローカル環境）は次のとおりです。値は環境ごとに変わります。

| 観測対象 | 正常 | 遅延あり |
|---|---:|---:|
| クライアントで測った応答時間 | 約33ms | 約617ms |
| Orderの予約処理 | 約14ms | 約0.6ms |
| PaymentのDB照会処理 | 約4ms | 約605ms |

**考えること：この遅さを直すために、Orderの在庫SQLを最初に最適化するべきでしょうか。**
今回の長い棒はPaymentのDB操作に集中しています。既知の実験条件ではDBロック待ちが原因です。
実際の障害では、このspanだけでロック・接続プール待ち・SQL実行時間を区別できません。次にDBの待機状況などを調べます。
トレーシングのメリットは、原因調査の対象を絞れることです。全ての原因を自動診断する機能ではありません。

## 5. タイムアウトと再開を比較する

「3 タイムアウト」を開きます。

- `payment.get`と`payment.store.get`が約1秒になっている。
- 失敗したspanにエラーの表示・`error=true`がある。
- `order.finalize`が存在しない。注文を確定するところまで進んでいない。

次に「4 同じ注文の手動再開」を開きます。

- 注文IDは3と同じ、trace_idは異なる。
- 入口は`POST /api/orders/{id}/resume`になっている。
- `order.reserve`はない。既存の予約を使っている。
- 今度は`order.finalize`まで進む。

**考えること：1本のトレースに注文の全生涯を入れるべきでしょうか。**
この構成ではトレースを1回の要求単位に区切ります。別時刻の再開・照会を注文IDで結び付けます。
デモスクリプトは4の再開APIを明示的に呼びます。バックグラウンドの自動再試行ではありません。

## 6. 配送失敗の補償を読む

「5 配送失敗の補償」を開くと、`payment.create`の後に`payment.cancel`が現れます。
最終的な注文は業務上の失敗ですが、取消・予約解除が正常完了している場合、通信障害を表すエラーspanはありません。
トレースのエラー有無だけで注文の成立を判断せず、アプリの注文状態も照合します。配送は引き続き模擬処理です。

## 7. ログとつなぐ

スクリプトが出した実際のIDへ置き換えます。

```sh
docker compose -p ec-tracing-demo logs --no-color --no-log-prefix api payment \
  | rg 'ここにtrace_idを貼る'
```

同じ実行のOrderとPaymentのJSONログが見つかります。`span_id`で、そのログがタイムラインのどの処理に属するか区別できます。
初回と再開をまとめて調べたいときは`order_id`で検索します。

| ID | 対象 |
|---|---|
| order_id | 注文という業務上の対象。初回・再開・照会で同じ |
| request_id | Orderが受けた1回のHTTP要求。下流Paymentにも渡す |
| trace_id | 分散した1回の実行のまとまり。traceparentでサービス間伝播 |
| span_id | その実行内の個別処理。HTTPクライアント・サーバー・DB操作など |

現在はGoの入口で新しいtraceを作りますが、有効なtraceparentを持つ呼出元ならそのtraceを引き継ぎます。
通常の画面・デモスクリプトはtraceparentを送らないため、手動再開は別traceになります。
JSONログへのID付与は実装済みですが、OTel Logsの転送・ログ検索基盤・メトリクス収集は今回の対象外です。

## 実装と制約

- SDK：OpenTelemetry Go 1.38.0。表示・受信：Jaeger 2.11.0（イメージdigest固定）。
- `backend/tracing.go`と`payment/tracing.go`：独立GoモジュールごとのSDK起動・バッチ転送・終了時flush、HTTP server span、ログ関連付け。
- `backend/payment.go`：応答本文の読取・契約検証までを含むHTTP client spanとtraceparentの送信。
- 注文の受付・予約・再開・確定、Paymentの作成・照会・取消DB操作にspanを追加。
- `order.reserve`は予約ヘルパーの時間で、呼出元のBEGIN/COMMITまでは含まない。SQL文単位の自動計装はない。
- service.nameはorder/payment。ローカル教材では全件記録する。Cookie・本文・SQL・接続文字列はトレースへ記録しない。
- 有限のバッチキューで非同期転送。Jaeger停止は業務要求を待たせないが、トレースの欠損はあり得る。監査台帳の代わりにはしない。
- `OTEL_EXPORTER_OTLP_ENDPOINT`未設定なら従来どおり動作し、SDKの転送を開始しない。
- UIをlocalhostに限定し、認証・永続保存・本番向けサンプリング設計は対象外。
- 分散トレーシングを追加しても、自動復旧や非同期注文にはならない。これらは学習4で扱う。

公式資料：[Go SDK](https://opentelemetry.io/docs/languages/go/)、[OTLP exporter](https://opentelemetry.io/docs/languages/go/exporters/)、[Jaeger](https://www.jaegertracing.io/docs/2.11/getting-started/)。

## 終了する

確認が終わったら、検証用コンテナーを停止します。DBのボリュームは保持します。

```sh
docker compose -p ec-tracing-demo down
```

この時点でJaegerのメモリ上のトレースは失われます。再デモは起動→スクリプト実行で作り直します。
通常環境は`docker compose up --build --wait`で起動し、アプリ3000番・Jaeger16686番を使います。
