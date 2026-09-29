# 0007: Orderの処理途中でPaymentを同期HTTP呼び出しし、結果不明を区別する

状態: 提案
日付: 2026-09-17
関連ADR: [0003: 注文と模擬処理を同一DBトランザクションで確定する](0003-monolith-order-transaction.md)、[0004: DB保存の匿名カートと複数明細の一括注文](0004-persistent-cart-multi-item-checkout.md)、[0006: 決済の責務を同期HTTPと専用DBを持つPaymentとして準備する](0006-standalone-payment-service.md)
人の採用記録: 会話で計画承認済み。本ADRの採用は未確認。

学習の最終確認: ユーザーが会話で「学習３は以上で問題ありません」と確認し、学習4への後処理を依頼。個別ADRの採用状態は引き続き提案。

## 背景と制約

[Issue #4](https://github.com/tokatu4561/ec-microservice/issues/4)の第1単位で、独立したPaymentと専用DBを作った。
今回の第2単位は、カート注文・単品注文から実Paymentへ接続し、画面の正常フローを維持する。
ユーザーには責務・ID・通信・ビルド・ログ・別DBのロールバック境界を説明し、
Orderの既存トランザクション内でHTTPを呼ぶ計画を提示したうえで「進めてください、実装後に解説してください」と承認を得た。

単一トランザクションの模擬決済はADR 0003・0004から変わるが、既存注文データや過去のPayment単体実験記録は移行しない。
OpenTelemetry、障害実験の詳細な実演、Saga・Outbox・自動再試行・結果不明時の永続的な復旧管理は対象外。
通常APIへ故障用パラメーターや故障用エンドポイントは作らない。

## 採用する判断

- Orderが注文IDを発行し、自身のDBの商品価格から金額を計算する。在庫不足ならPaymentへ要求を送らない。
- `paymentGateway`を明示的に注入し、通常バイナリはHTTP実装のみを使う。`PAYMENT_BASE_URL`は必須で、Composeでは`http://payment:8080`を指定する。
- 単品・カートとも共通の`createOrderWithPaymentTx`で決済・必要時の取消を呼び出す。
- 既存のロック順とOrder側トランザクションを維持し、その途中で同期HTTPを呼ぶ。モノリスとの差を小さくして比較し、通信待ちがロック保持に与える影響を学べるようにする。
- 通信は1回あたり1秒、親の注文処理は従来どおり3秒。親キャンセルを下流HTTPへ伝え、本文読み取りにも通信期限を適用する。リダイレクト追従・自動再送・旧模擬処理へのfallbackはしない。
- Paymentの201＋`failed`は明確な業務失敗として従来どおり失敗注文を保存する。配送失敗ではPayment取消の200＋`cancelled`を確認してから失敗注文を保存する。
- 成功扱いのHTTP応答でも、注文ID・金額・状態・JSON形式が契約を満たすか確認する。異常HTTP、通信エラー、契約違反は決済失敗と断定せず503＋注文IDを返す。
- Order側DBのエラーではOrder側のロールバックを試みる。Paymentのコミットは別DBのため戻らない。DB保存結果が不明なまま無条件に決済取消することも避ける。
- 画面には「注文が見つからなくても未決済とは限らない」と表示し、注文IDと問い合わせ用IDで両方の結果を照合するよう案内する。
- Orderで生成するrequest_idをPaymentへの`X-Request-ID`で引き継ぐ。相関ログは追加するが、trace_id/span_id/traceparentを導入済みとは扱わない。
- 既存のOrder DBテスト・学習2のSQL比較は`*_test.go`だけの模擬依存で保持し、実Paymentを使う別DB間の連携テストを追加する。テスト用の模擬実装は通常バイナリへ含めない。

## 比較案と見送った理由

- 最初から注文を先に永続化し、HTTPの前後でDBトランザクションを分ける：ロックを短くできるが、途中状態・在庫確保・カート・再開処理の設計まで同時に必要になる。今回は小さな比較単位を選び、後続で検討する。
- 通信エラーを`payment_failed`として保存：決済の成功応答だけを失ったケースで事実と異なる状態になるため採用しない。
- Payment停止時に旧模擬決済へ切り替える：決済記録を新サービスに残さず成功に見せてしまうため採用しない。
- 注文保存エラー時に常に取消を呼ぶ：Order側のCOMMIT結果自体が不明な場合、成立済み注文の決済を取り消す可能性があり、確実な復旧管理なしには行わない。
- 今回同時にOpenTelemetry・Sagaまで実装：ユーザーと合意した区切りに従い、HTTP連携、観測機能、障害実験・復旧設計を段階的に扱う。

## 利点と不利益

画面から実際に独立Paymentを呼び、決済結果を各DBと相関ログで照合できる。
一方、通信中に在庫・カートのロックを保持するため、Paymentの遅延が同じ商品・カートの処理待ちへ波及する。
Payment停止中は商品一覧等を使えても、新しい購入は成立しない。

結果不明時に注文が残らずPaymentだけに記録が残ることがある。Orderには永続的な要対応リストがなく、
現在は返されたID・ログで照合する。通信でエラー応答も失われると、画面がIDを取得できない場合もある。
カートに商品が残っていても、自動再送はしない。手動の再注文は新しい注文IDとなるため、結果照合前の再注文を防ぐ完全な仕組みにはなっていない。
実際の課金・返金は行わず、実外部決済の安全性や本番向け整合性を保証する設計ではない。

## 見直す条件

- 次の観測単位でOpenTelemetryを導入し、HTTP待ち・サービス内部処理をトレースとログで関連付ける。
- ロック保持が問題になる場合、HTTP前後のトランザクション分離と、注文・在庫・カートの途中状態を設計する。
- 途中停止からの復旧や実外部決済を扱う際、冪等性、永続的な進行状態、照合・再試行・補償の仕組みを設計する。
- 決済の複数試行を扱う際は、注文ID主キーの1注文1記録というPayment側制約を見直す。

## 根拠と関連資料

- 2026-09-17の会話：Order→Payment同期HTTP、1秒期限、結果不明の503、通信中のロック保持、OpenTelemetryを次単位にする計画を提示し、ユーザーが承認。
- 基準commit：`28612436ed85e6da76d118bb8b8306c705e588d1`。branch：`codex/learning3-payment`。前単位の未コミット変更も保持し、レビュー対象へ含めた。
- [HTTPクライアント](../../backend/payment.go)、[注文処理](../../backend/orders.go)、[カート](../../backend/cart.go)、[画面](../../frontend/app/orders.tsx)、[教材と図](../learning3-order-payment.md)。
- `docker compose -p ec-learning3-link-verify --profile test run --build --rm api-test`：全既存テスト＋別DB間18ケース、HTTP契約、期限・キャンセル、race、整形、vet、ビルドが成功。
- 同プロジェクトの`payment-test`：全単体・DB・並行テスト、race、整形、vet、ビルドが成功。
- `WEB_PORT=3002 PAYMENT_PORT=8082 docker compose -p ec-learning3-link-verify up --build --wait --wait-timeout 120`：成功。webのTypeScript・ESLint・本番ビルドを実行。
- `BASE_URL=http://127.0.0.1:3002 python3 scripts/smoke.py`：成功。
- `COMPOSE_PROJECT_NAME=ec-learning3-link-verify WEB_PORT=3002 PAYMENT_PORT=8082 BASE_URL=http://127.0.0.1:3002 PAYMENT_BASE_URL=http://127.0.0.1:8082 python3 scripts/order_payment_smoke.py --lifecycle`：成功。正常・明確失敗・取消・停止時503・復旧を確認。
- 同じ検証環境の`python3 scripts/payment_smoke.py --lifecycle`：成功。Payment・DB再起動で記録を保持。
- Playwright実ブラウザーで980円の注文成功、カート消去、Payment停止時503とID・結果確認案内、注文照会404でも未決済と断定しない表示を確認。記録は`output/playwright/learning3-link-cli-20260917/`。
- 正常注文`b6a14eb16789b2b8ba3dec369e744ad6`のrequest_id `b2ac992961ea5d34b022d08de87fe176`をOrder・Paymentの実ログで照合。
- 故障注入テスト注文`db51e2d699ed7da95951d6fcb71c2442`をDBで再確認し、Paymentは500円・succeededの1行、Orderは0行。テストでは在庫・カートの復元も確認。
- 修正1/2：通信期限テストのHTTPサーバーの終了待ちを明示releaseで解消。アプリ側は当初から約1秒で期限切れ。検証条件は維持し、全テストを再実行して成功。
- 新しいreviewerのレビューは必須指摘・任意提案ともになし。修正回数は前単位から継続して1/2。Python構文・差分・教材リンク検査も成功。
- 本ADRはレビュー後に会話・実装・検証と照合して整理。GitHub Actionsの実行、人の最終確認・ADR採用は未確認。
