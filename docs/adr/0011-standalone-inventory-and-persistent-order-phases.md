# 0011: Inventoryを専用DBへ分離し、注文の進行段階を永続化する

状態: 提案
日付: 2026-09-29
関連ADR: [0008: 短いTXと手動復旧](0008-short-transactions-and-manual-order-recovery.md)、[0009: OpenTelemetry](0009-distributed-tracing-with-opentelemetry.md)、[0010: Shipping分離](0010-standalone-shipping-and-manual-recovery.md)
人の採用記録: 実装計画は会話で承認済み。本ADRの採用は未確認。

学習の最終確認: ユーザーが会話で「学習３は以上で問題ありません」と確認し、学習4への後処理を依頼。個別ADRの採用状態は引き続き提案。

## 背景と制約

[Issue #4](https://github.com/tokatu4561/ec-microservice/issues/4)の学習として、Payment・Shippingに続きInventoryを分離する。
会話では、現在の規模で運用上必須の分離ではなく、注文保存と在庫予約を一つのDBトランザクションにできなくなった際の整合性と復旧を学ぶため、と説明した。独立サービス、予約の確定・解除、永続的な進行段階、オフライン移行の計画を提示し、ユーザーが実装を承認した。

ローカルComposeと模擬決済・配送を継続する。Queue、自動再試行、予約期限、複数倉庫、実在庫台帳、商品補充APIは対象外。旧Orderとの混在運用・移行後のダウングレードも対象外。
この判断はADR 0008・0010時点の「OrderのDB内で在庫を予約・解除する」構成を変更する。以前のADRは当時の判断記録として保持する。

## 採用する判断

- Inventoryを独立したGoモジュール・Dockerイメージ・プロセス・PostgreSQLにする。販売可能数`stocks.available`と予約`reservations`を所有する。商品名・価格・注文・注文明細・進行段階はOrderに残す。
- 通常のOrderプロセスはInventory DBに接続せず、`inventoryGateway`インターフェース経由でHTTPクライアントを使用する。専用内部ネットワークでDBを分離する。両DBの接続情報を持つのは移行専用コマンドだけとする。
- 一注文一予約として`order_id`を予約の主キーにする。他DBの注文への外部キーは持たない。商品ID・数量を正規化し、同じ内容の再送は現在の予約を返し、異なる内容は409とする。DBトランザクションIDの共有ではない。
- 予約は複数商品を単一TXで確認し、商品ID順にロックする。在庫不足なら全商品の数量を維持して`rejected`を保存する。予約時だけ販売可能数を減らし、`committed`への確定では減らさない。`released`への解除で一度だけ戻す。確定と解除の競合は一方だけ成功し、反対の操作は409とする。
- 在庫不足・確定・解除後の予約を、同じ注文の再送で復活させない。新たな購入は新しい注文とする。
- Orderは外部通信前に注文・明細・`inventory_pending`を保存する。予約→決済→配送受付→予約確定→注文確定の各段階を永続化する。段階を比較して更新し、並行再開の古い結果で進んだ状態を上書きしない。
- 外部HTTP中にOrderのDB接続・TXを保持しない。各通信は最大1秒、要求全体にも既存の期限を適用する。自動リトライ・リダイレクト・旧ローカル在庫処理への切替はしない。
- 404だけで過去の要求の未処理を断定せず、同じ内容を安全に再要求できる契約と組み合わせる。タイムアウト・通信断・不正応答は結果不明として注文と段階を保持し、手動のresumeで続行する。
- 決済が明確に失敗した場合は予約を解除する。配送受付が明確に失敗した場合はPayment取消を確認してから予約を解除する。結果不明のまま数量を戻さない。成功・失敗の最終確定までカートを処理中として保持する。
- 商品一覧・カートの在庫は一括HTTP照会する。取得不能は`stockKnown=false`として画面に「在庫確認不可」を表示し、在庫ゼロと区別する。保存済みのカート編集を、照会不能だけを理由に失敗としない。
- 移行前に旧Orderを停止する。既存`products.stock`は予約分を控除済みの販売可能数として転記し、保持中の予約は記録だけを追加して二重減算しない。入力の指紋と移行マーカーを両DBに保存し、再実行でInventoryの現在数量を上書きしない。旧列は移行元・過去教材用に残すが、新アプリの数量取得・更新には使用しない。
- OpenTelemetryのHTTP伝播で4サービスを関連付ける。JSONログにrequest_id・order_id・trace_id・span_idを記録する。応答の意味検証までをクライアントspanに含める。トレース転送は非同期バッチ、業務HTTPは同期のままとする。

## 比較案と見送った理由

- InventoryをOrder内に維持する：現在の規模では十分合理的で、単一TXによる整合性と運用の単純さを保てる。今回はDB境界を越えた結果不明と復旧を学ぶ目的で分離する。
- OrderからInventoryのテーブルを直接更新する：サービスのデータ所有境界が崩れ、独立したAPI契約・障害を学ぶ目的に合わない。
- 通信失敗時に即座に注文失敗・在庫解除にする：相手で予約・決済・配送が保存済みなら、不整合や二重販売を招き得る。
- Queue・自動復旧・期限切れ解放も同時導入する：学習範囲が広がるため、まず同期HTTPで障害と手動復旧を観察する。自動化は学習4で扱う。
- 毎回すべてのサービスを最初から確認する：永続段階から再開することで、確認済みの工程への不要な通信を避け、補償の進行を明確にする。

## 利点と不利益

在庫の更新責任を分離でき、Inventoryだけの停止・更新・競合を検証できる。予約の冪等性とOrderの永続段階により、応答喪失やOrder更新失敗から同じ注文を再開できる。

一方、DB・サービス・通信・状態が増え、単一TXの原子性は失われる。Inventory停止は数量表示と注文進行に影響する。手動再開しなければ保留注文・予約は残る。同期通信と複数の状態保存により遅延と障害点も増える。
移行には停止時間が必要で、単純な旧版への切戻しはできない。一注文一予約は分割出荷・予約変更には不足する。分離による性能改善や本番運用上の必要性を証明したものではない。

## 見直す条件

- 学習4でQueue・Outbox・Saga・回数上限付きRetry・Backoff・DLQ・自動復旧を導入する時。
- 予約期限、注文取消、分割出荷、商品補充、複数倉庫を扱う時。
- 停止を伴わない移行、新旧混在、本番運用を扱う時。
- サービス数による運用負担が責務分離の利点を上回る時。

## 根拠と関連資料

- 会話：Inventory分離の学習目的、具体的な実装範囲、移行と結果不明時の判断を説明し、ユーザーが承認。Inventory新単位として最大2修正ラウンドの例外も承認済み。
- [Inventory実装](../../inventory/store.go)、[移行](../../inventory/import.go)、[HTTPクライアント](../../backend/inventory.go)、[Order進行](../../backend/order_progress.go)、[migration](../../db/migrations/006_inventory_progress.sql)、[図とデモ手順](../learning3-inventory.md)。
- 基準commit `28612436ed85e6da76d118bb8b8306c705e588d1`、branch `codex/learning3-payment`。前単位からの未コミット変更を保持。
- `COMPOSE_PROJECT_NAME=ec-inventory-test docker compose --profile test run --build --rm inventory-test`：複数商品・在庫競合・冪等予約・確定解除競合・移行・トレース、gofmt・race・vet・build成功。
- 同環境の`api-test`：既存回帰、応答喪失・Order更新失敗・8並列再開、HTTP契約と結果不明のログ・トレース、gofmt・race・vet・build成功。
- 専用環境`ec-inventory-demo`のNext.js型検査・lint・本番build、全サービス起動成功。web3004、Payment8087、Shipping8088、Inventory8089、Jaeger16689。
- `scripts/inventory_demo.py --lifecycle`：正常・決済失敗・配送失敗・在庫不足、Inventory停止時の不明表示と決済未開始、Order再起動後の再開、Inventory/DB再起動、再importで上書きなし、4サービスのtraceと親子関係が成功。
- `scripts/smoke.py`、`scripts/order_payment_smoke.py --lifecycle`、`scripts/shipping_demo.py --lifecycle`、`scripts/tracing_demo.py`の既存回帰デモも成功。
- 初回レビューのP2は、実storeの子span追加に対する期待と、旧比較用ヘルパーのInventoryStatus既定値の不一致。検証内容を維持して修正1/2で解消した。
- 新規レビュー担当の起動はエージェント数上限で失敗。ユーザーが今回限り既存Inventory担当の再利用を明示承認し、修正後全体を再レビューした。結果は必須指摘なし。修正回数はInventory単位1/2。
- P3保留：READMEの注文状態の一覧に`release_pending`が未記載。API・画面・本設計資料は対応済み。
- `git diff --check`・Compose構成確認・Python構文確認は成功。GitHub Actionsのリモート実行、人の最終確認、ADR採用は未確認。commit・push・mergeは行っていない。
