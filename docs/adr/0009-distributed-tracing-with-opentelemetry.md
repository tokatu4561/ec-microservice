# 0009: OpenTelemetryでOrderとPaymentを計装し、JaegerとJSONログで処理を追跡する

状態: 提案
日付: 2026-09-24
関連ADR: [0007: 同期HTTP連携](0007-order-payment-synchronous-http.md)、[0008: 短いTXと手動復旧](0008-short-transactions-and-manual-order-recovery.md)
人の採用記録: 実装範囲とデモ手順の作成は会話で承認済み。本ADRの採用は未確認。

学習の最終確認: ユーザーが会話で「学習３は以上で問題ありません」と確認し、学習4への後処理を依頼。個別ADRの採用状態は引き続き提案。

## 背景と制約

[Issue #4](https://github.com/tokatu4561/ec-microservice/issues/4)の分散処理を観測する単位。
これまではorder_id/request_idでログを関連付けていたが、処理の親子関係や時間配分は人が組み立てる必要があった。
ユーザーにOTel・ログ関連付け・ローカル表示・正常/遅延/障害/再開の比較を提示し、実装とデモ手順の作成を承認された。
自動復旧・非同期注文・OTel Logs転送・メトリクス基盤・本番運用は対象外。

## 採用する判断

- OrderとPaymentの独立GoモジュールへOpenTelemetry Go 1.38.0を追加し、起動・終了をそれぞれ管理する。
- W3C TraceContextでHTTP受信時に親を抽出し、PaymentへのHTTP要求にtraceparent/tracestateを注入する。Baggageは扱わない。
- HTTP server/client spanと、注文受付・予約・再開・確定、PaymentのDB作成・照会・取消に手動のspanを配置する。
- 決済クライアントは応答本文の読取・契約検証まで計測するため、業務操作単位のclient spanで包む。HTTP transportの二重計装はしない。
- 正常な新規決済照会の404は障害扱いしない。通信・契約違反・DBエラーはErrorとして記録し、業務上の注文失敗と区別する。
- route名はIDを埋め込まないテンプレートで表示し、order_id/request_idを属性として付ける。本文・Cookie・SQL・接続文字列をトレースへ含めない。
- slogのContext対応Handlerで既存JSONログへtrace_id/span_idを追加する。ログ自体のOTLP転送とは分ける。
- Jaeger 2.11.0をdigest固定でComposeに追加し、OTLP/HTTPを内部4318で受信。UIだけを127.0.0.1へ公開する。
- ローカル教材は全件記録する。有限キューでバッチ転送し、収集先の停止によって注文応答を待たせない。終了時は独立した短いcontextでflushする。
- 通常の画面からの初回注文・再開は別traceとし、同じorder_idで関連付ける。traceparentを持つ上流要求は継承する。
- 遅延・タイムアウトは専用DBの短時間ロックで再現する。通常APIへ障害注入機能は追加しない。

## 比較案と見送った理由

- 相関ログだけを継続：注文全体のログ検索はできるが、親子関係と時間配分の理解を助ける可視化が不足する。
- 注文の全生涯を1本のtraceへ固定：別時刻の照会・再開が長大になる。業務IDと実行IDを分ける学習目的に合わせ、要求ごとに分ける。
- Collector・ログ検索基盤・メトリクス基盤も同時に導入：構成と運用の学習範囲が広がるため、今回はJaegerへ直接OTLP転送する。
- DBの全SQLを自動計装：今回の目的はサービス境界と処理段階の理解であり、業務操作単位のspanに限定する。
- 収集失敗時に注文を失敗させる：観測系の停止を購入処理へ波及させない方針から採用しない。

## 利点と不利益

OrderからPaymentへの親子関係、待ち時間、失敗箇所、再開・補償経路を可視化できる。ログを個別spanへ結び付けられる。
一方、依存ライブラリ・収集プロセス・計装コードが増え、全件記録には資源コストがある。
独立モジュールに小さなtracing bootstrap/Handler実装が重複する。今は別ビルドを保ち、共通モジュールの配布設計は導入しない。

Jaegerはメモリ保存で再起動時に消える。バッチキューが溢れたり、収集先が長く停止するとトレースが欠損しうる。監査証跡の保証ではない。
ブラウザー・Next.jsのspan、SQL単位の時間、メトリクスはない。payment.store.getが遅いだけでは、ロック・接続取得・SQL実行のどれが原因か確定しない。
order.reserveはヘルパーの実行時間で、呼出元のBEGIN/COMMIT時間を含まない。

## 見直す条件

- 本番・高負荷へ進むとき：サンプリング、保存期間、永続ストレージ、認証、属性の扱い、収集系の容量を設計する。
- Queue・ワーカーを追加するとき：メッセージへのcontext伝播、再配送とspan link、別実行の関連付けを設計する。
- DB内部の分析が必要なとき：DBメトリクス・待機調査・必要な範囲のSQL計装を追加する。
- 複数の出力先・ログ/メトリクスが必要なとき：Collectorやログ基盤の採否を比較する。

## 根拠と関連資料

- 会話の計画提示とユーザーの「進めてください」「完了後に…デモ手順を用意」に基づく。
- [Order計装](../../backend/tracing.go)、[Payment計装](../../payment/tracing.go)、[デモ](../learning3-tracing.md)、[スクリプト](../../scripts/tracing_demo.py)。
- 公式資料：[Go SDK](https://opentelemetry.io/docs/languages/go/)、[exporters](https://opentelemetry.io/docs/languages/go/exporters/)、[Jaeger](https://www.jaegertracing.io/docs/2.11/getting-started/)。
- 基準commit `28612436ed85e6da76d118bb8b8306c705e588d1`、branch `codex/learning3-payment`。前単位からの未commit差分・追加ファイルもレビュー対象。
- `docker compose -p ec-tracing-test --profile test run --build --rm api-test`と`payment-test`：全テスト・race・gofmt・vet・build成功。trace親子関係・受信context・JSONログの実spanID一致を単体テストで確認。
- `WEB_PORT=3002 PAYMENT_PORT=8082 JAEGER_PORT=16687 docker compose -p ec-tracing-demo up --build --wait --wait-timeout 120`：起動成功、web型検査/lint/build成功。
- `python3 scripts/tracing_demo.py --project ec-tracing-demo`：5ケース成功。Jaegerの両サービス・親子関係・エラー、同一注文別traceでの再開、ログのtrace/span IDを照合。
- 実測：正常約33ms、遅延約617ms（Payment DB操作約605ms）、タイムアウト約1008ms、再開約16ms。性能保証値ではない。
- `scripts/smoke.py`成功。`scripts/order_payment_smoke.py --lifecycle`成功（計装後もPayment停止・Order再起動から復旧）。
- Jaegerをpauseした状態でも注文`8ae4083cab0b4c82035651d87d105e10`が成功し、unpauseして復旧。
- PlaywrightでJaegerの遅延traceを表示し、2サービス・11span・全体613msとDB操作の長い棒を確認。参考画像は`output/playwright/tracing-20260924/slow-trace.png`（非追跡の検証成果物）。
- reviewerの結論は必須指摘なし。P3として、デモのログspan_idをJaegerの実spanID集合とも照合すると検証が強くなるとの提案あり（単体テストでは一致を確認済み）。
- 修正回数は前単位から継続1/2。今回の修正ラウンドは0回。GitHub Actions実行、人の最終確認、ADR採用は未確認。
