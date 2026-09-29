# 0010: Shippingを同期HTTPと専用DBへ分離し、結果不明の注文を手動再開する

状態: 提案
日付: 2026-09-26
関連ADR: [0008: 短いTXと手動復旧](0008-short-transactions-and-manual-order-recovery.md)、[0009: OpenTelemetry](0009-distributed-tracing-with-opentelemetry.md)
人の採用記録: 実装計画は会話で承認済み。本ADRの採用は未確認。

学習の最終確認: ユーザーが会話で「学習３は以上で問題ありません」と確認し、学習4への後処理を依頼。個別ADRの採用状態は引き続き提案。

## 背景と制約

[Issue #4](https://github.com/tokatu4561/ec-microservice/issues/4)のサービス分割をPaymentに続けて学ぶ。
2026-09-25〜26の会話で、Shipping、Inventory、全体障害実験の順に学習3を拡張する方針を提示し、Shippingの具体的な計画と役割の解説後、ユーザーが「OK、ではその方針で進めてください」と承認した。
今回の単位はShippingのみ。Inventoryは引き続きOrder内。Queue、Outbox、自動再試行、実配送、出荷・配達状態、注文全体の取消UIは対象外。
既存注文・未コミット変更を保持し、独立したデモ環境で検証する。

## 採用する判断

- `shipping/`を独立したGoモジュール、Dockerイメージ、プロセス、PostgreSQLにする。Shipping専用の内部DBネットワークを用い、OrderはDBを直接参照しない。
- 一注文一配送を契約とし、`shipments.order_id`を主キーにする。注文DBへの外部キーは持たない。配送対象の商品ID・数量・模擬モードを保存する。価格・住所は今回の契約に含めない。
- 作成・照会・取消のHTTP APIを公開。明細を商品ID順へ正規化し、同一内容の再送は同じ記録の現在状態を返す。異なる明細・モードは409。取消済みの依頼は再作成で復活しない。
- 模擬受付の状態は`requested`・`failed`・`cancelled`。`requested`は配達完了を意味しない。取消は作成済みの受付に対してのみ行い、未作成は404、失敗済みは409。未作成への取消によって後続の作成を禁止する契約ではない。
- OrderはshippingGatewayインターフェースに依存し、実行時にHTTPクライアントを注入する。各HTTP呼出しは1秒、リダイレクト・自動再試行は行わない。通常バイナリに旧模擬処理へのフォールバックは組み込まない。
- 決済成功確認後、Orderに`processing/succeeded/pending`を保存し、Shippingの照会・必要なら同一要求で作成を行う。404だけで未処理と断定せず、冪等なPOST契約で遅延中の要求との競合に対応する。
- 通信断・不正応答・タイムアウトは結果不明として注文・決済・在庫予約を保持する。明確な`failed`を確認した場合に限り`cancel_pending`を保存してPaymentを取消し、取消確認後に短いTXで在庫解除・失敗確定する。
- 再開は保存済みの注文明細・モードを使用する。終端状態はそのまま返す。旧`cancel_pending`はShippingへ依頼せず既存の取消処理を再開する。並行再開時の最終更新と在庫解除は注文行ロックで一度に限定する。
- 配送取消APIは単体学習用。完了注文の配送を直接取り消してOrderを自動更新する機能はない。予期しない配送`cancelled`はOrderで要確認とする。
- 005 migrationで配送確認待ちを許可する。migrationを全件再実行する既存運用に合わせ、004のチェック制約も同じ状態を許可する。旧Orderを止めて移行し、新Orderを起動する。旧版との混在・ダウングレードは今回扱わない。
- Order→ShippingへW3C TraceContextを伝播し、ShippingのHTTP・DB操作もJaegerで追跡する。JSONログにはtrace_id/span_idを付ける。トレース転送は従来と同じ有限キュー・非同期バッチで、ログは標準出力に残す。

## 比較案と見送った理由

- Queueを同時導入：非同期配送・再試行・Outboxまで学習範囲が広がる。同期HTTPの分離と結果不明を先に体験し、学習4で自動化する。
- Shippingタイムアウト時に即座に決済取消・在庫解除：配送が保存済みなら未払い配送や在庫の二重販売につながり得る。結果不明は保持する。
- OrderとShippingの共有DB：配送の更新責任と独立した障害を体験する目的に合わない。
- 最初から分割配送・配送IDの複数発行を導入：今回の一注文一配送の教材に不要。複数配送が必要になったら注文IDと配送IDを分ける。
- 起動時に既存の完了注文から配送記録を生成：意図しない再依頼を避け、既存終端注文は保持する。

## 利点と不利益

Shippingだけを停止・更新でき、配送受付の記録と責任を分離できる。決済成功後の配送結果不明、重複要求、再開を実物のAPI・DB・トレースで確認できる。
一方、コンテナ・DB・通信・状態遷移が増える。人が再開しなければ予約が残り、依存サービス停止時の注文完了には復旧が必要。
同期HTTPが増えるため、呼出し単位の1秒に加えOrder要求全体の3秒期限も影響する。完了せず確認待ちになった場合は同じ注文を再開する。
配送住所や出荷工程を扱わない模擬受付であり、本番物流の安全性・性能を保証する構成ではない。

## 見直す条件

- Inventory分離時：在庫予約の所有DB、予約ID、確定・解除、途中失敗からの復旧を具体化する。
- 学習4：Queue・Outbox・Saga、再試行上限・Backoff・DLQ、自動復旧を導入する。
- 注文取消・出荷・分割配送を扱う時：配送ID、取消可能時点、遅延要求との競合、Orderとの整合性を再設計する。
- 本番運用時：認証・認可・配送先情報、秘密情報管理、DB移行運用、観測データの保存と費用を設計する。

## 根拠と関連資料

- 会話：Shipping→Inventoryの順序、実装計画・役割の説明とユーザー承認。設計理由は実装前・途中の会話に記録。
- [Shipping実装](../../shipping/store.go)、[HTTPクライアント](../../backend/shipping.go)、[Order復旧](../../backend/order_progress.go)、[migration](../../db/migrations/005_shipping_progress.sql)、[学習・デモ手順](../learning3-shipping.md)。
- 基準commit `28612436ed85e6da76d118bb8b8306c705e588d1`、branch `codex/learning3-payment`。前単位からの未コミット差分と追加ファイルをレビュー対象に含めた。
- `COMPOSE_PROJECT_NAME=ec-shipping-test docker compose --profile test run --build --rm shipping-test`：API・DB・並行作成/取消・トレース、race・gofmt・vet・build成功。
- `COMPOSE_PROJECT_NAME=ec-shipping-clean docker compose --profile test run --build --rm api-test`：既存テスト、migration再適用、Shipping結果不明・8並列再開、race・gofmt・vet・buildすべて成功。
- 初回の新統合テストはカートcleanup漏れで失敗。削除順を修正した。中間再検証は初回の残存データで商品数テストが失敗したため、クリーンな専用DBで全件検証した。受入条件・アサーションの緩和はしていない。
- 初回デモは非同期トレース未到着404で停止。404のみを待機対象へ修正し、期待する3サービスと親子関係の検証を維持した。
- デモ専用Compose：web3003、Payment8084、Shipping8085、Jaeger16688。Next.js型検査・lint・build成功。
- `scripts/shipping_demo.py --lifecycle`：正常・受付失敗・同一要求/異内容・単体取消後再送・Shipping停止・Order再起動・手動復旧・ShippingとDB再起動後の永続化・3サービストレース成功。
- 既存の`scripts/smoke.py`、`scripts/order_payment_smoke.py --lifecycle`、`scripts/tracing_demo.py --project ec-shipping-demo --base-url http://127.0.0.1:3003 --jaeger-url http://127.0.0.1:16688`も成功。
- Playwright：Shipping停止時の決済成功/配送確認待ち・再注文防止、復旧後の同注文再開、配送失敗時の取消表示を確認。注文`69184840a959d6c0756aa25bd62bf4b7`の再開成功。Jaeger正常trace `ebaa78471ec2e2924b805c11c3578e0a`は3サービス・17スパン・19.37ms（性能保証値ではない）。
- 初回レビューP2の2件を修正2/2で対応。新しいコンテキストの再レビューは必須指摘なし。累計修正回数2/2。
- P3保留：READMEのログ例に旧イベント名が残る。今後の資料整理事項であり、今回追加修正はしない。
- Python構文確認・`git diff --check`成功。GitHub Actions実行・人の最終確認・ADR採用は未確認。commit/pushは行っていない。
