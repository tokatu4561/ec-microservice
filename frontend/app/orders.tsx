"use client";

import Link from "next/link";
import { useEffect, useState } from "react";
import { useRouter } from "next/navigation";
import { api, ApiError, type Order } from "./api";

const paymentLabels = { pending: "確認待ち", not_started: "未実施", succeeded: "成功", failed: "失敗", cancelled: "取消済み" };
const shippingLabels = { pending: "受付の確認待ち", not_started: "未実施", requested: "依頼済み", failed: "失敗" };
const reasons = { out_of_stock: "在庫が不足しています。", payment_failed: "模擬決済が失敗しました。在庫は戻しています。", shipping_failed: "模擬配送が失敗しました。模擬決済を取り消し、在庫を戻しています。" };

const inventoryLabels = {legacy:"移行前の完了記録",pending:"予約の確認待ち",reserved:"予約済み",commit_pending:"販売確定の確認待ち",release_pending:"解除の確認待ち",committed:"販売確定",released:"解除済み",rejected:"在庫不足"};

const statusLabels = { shipping_requested: "配送依頼済み", failed: "失敗", processing: "処理中・確認待ち", cancel_pending: "決済取消の確認待ち", release_pending: "在庫解除の確認待ち" };

export function OrderSummary({ order }: { order: Order }) {
  return <article className={`order-result ${order.status === "failed" ? "error" : ""}`} aria-label="注文結果">
    <h3>{order.status === "shipping_requested" ? "注文を受け付けました" : order.status === "failed" ? "注文は成立しませんでした" : "注文処理の確認・再開が必要です"}</h3>
    {order.failureReason && <p>{reasons[order.failureReason]}</p>}
    <ul className="order-items">{order.items.map((item) => <li key={item.productId}>
      <span>{item.productName} × {item.quantity}点</span>
      <span>単価 ¥{item.priceYen.toLocaleString("ja-JP")} / 小計 ¥{item.subtotalYen.toLocaleString("ja-JP")}</span>
      {item.stockShortage && <strong>在庫不足</strong>}
    </li>)}</ul>
    <dl>
      <dt>注文ID</dt><dd className="order-id">{order.id}</dd>
      <dt>注文合計</dt><dd>¥{order.totalYen.toLocaleString("ja-JP")}（税込）</dd>
      <dt>状態</dt><dd>{statusLabels[order.status]}</dd>
      <dt>在庫</dt><dd>{inventoryLabels[order.inventoryStatus]}</dd>
      <dt>模擬決済</dt><dd>{paymentLabels[order.paymentStatus]}</dd>
      <dt>模擬配送</dt><dd>{shippingLabels[order.shippingStatus]}</dd>
      <dt>受付日時</dt><dd>{new Date(order.createdAt).toLocaleString("ja-JP", { timeZone: "Asia/Tokyo" })}（日本時間）</dd>
    </dl>
  </article>;
}

export function ErrorMessage({ error, submission = false }: { error: Error; submission?: boolean }) {
  const uncertain = submission && (!(error instanceof ApiError) || error.status >= 500);
  return <div className="notice error" role="alert">
    <p>{error instanceof ApiError ? error.message : "通信に失敗しました。接続を確認してください。"}</p>
    {uncertain && <p>注文・決済・配送依頼が保存されている可能性があります。自動再送はしていません。再注文せず、注文状況を確認して同じ注文の処理を再開してください。</p>}
    {error instanceof ApiError && error.requestId && <p className="request-id">お問い合わせ用ID: {error.requestId}</p>}
    {error instanceof ApiError && error.orderId && <Link href={`/orders/${error.orderId}`}>注文ID {error.orderId} の状況を確認</Link>}
  </div>;
}

export function OrderLookup() {
  const [id, setID] = useState("");
  const router = useRouter();
  return <section className="panel" aria-labelledby="lookup-heading">
    <h2 id="lookup-heading">注文状況を確認する</h2>
    <form onSubmit={(e) => { e.preventDefault(); router.push(`/orders/${id.trim()}`); }}>
      <label>注文ID<input value={id} onChange={(e) => setID(e.target.value)} pattern="[0-9a-f]{32}" maxLength={32} required placeholder="注文結果に表示された32文字のID" /></label>
      <button type="submit">状況を確認</button>
    </form>
  </section>;
}

export function OrderDetails({ id }: { id: string }) {
  const [order, setOrder] = useState<Order | null>(null);
  const [error, setError] = useState<Error | null>(null);
  const [attempt, setAttempt] = useState(0);
  const [busy,setBusy] = useState(false);
  async function resume() {
    setBusy(true); setError(null);
    try {
      const result=await api<{order:Order}>(`/api/orders/${encodeURIComponent(id)}/resume`,{
        method:"POST",headers:{"Content-Type":"application/json","X-Cart-Request":"1"},body:"{}",
      });
      setOrder(result.order);
    } catch(err) { setError(err instanceof Error ? err : new Error("通信エラー")); setAttempt(n=>n+1); }
    finally { setBusy(false); }
  }
  useEffect(() => {
    const controller = new AbortController();
    async function load() {
      try {
        const result = await api<{ order: Order }>(`/api/orders/${encodeURIComponent(id)}`, {
          signal: AbortSignal.any([controller.signal, AbortSignal.timeout(8000)]),
        });
        if (!controller.signal.aborted) setOrder(result.order);
      } catch (err) { if (!controller.signal.aborted) setError(err instanceof Error ? err : new Error("通信エラー")); }
    }
    void load();
    return () => controller.abort();
  }, [id, attempt]);
  return <>
    {!order && !error && <p role="status">注文を読み込んでいます…</p>}
    {order && <OrderSummary order={order} />}
    {order && (order.status==="processing" || order.status==="cancel_pending" || order.status==="release_pending") && <div className="notice">
      <p>在庫予約などの結果を確認しています。再注文せず、この注文の処理を再開してください。接続先が復旧するまで結果が確定しない場合があります。</p>
      <button disabled={busy} onClick={resume}>{busy ? "確認しています…" : "処理を再開"}</button>
    </div>}
    {error && <><ErrorMessage error={error} />{error instanceof ApiError && error.status === 404 && <p className="notice">注文が見つからなくても、決済が成功している可能性があります。再注文する前に、この注文IDで決済状況を確認してください。</p>}</>}
    <button disabled={busy} onClick={() => { setError(null); setOrder(null); setAttempt((n) => n + 1); }}>状況を再取得</button>
  </>;
}
