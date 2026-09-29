"use client";

import Link from "next/link";
import { useEffect, useRef, useState } from "react";
import { api, type Cart, type Product, type Order } from "./api";
import { ErrorMessage, OrderLookup, OrderSummary } from "./orders";

export default function Home() {
  const [products, setProducts] = useState<Product[]>([]);
  const [cart, setCart] = useState<Cart | null>(null);
  const [busy, setBusy] = useState(false);
  const busyRef = useRef(false);
  const [error, setError] = useState<Error | null>(null);
  const [submissionError, setSubmissionError] = useState(false);
  const [order, setOrder] = useState<Order | null>(null);
  const [quantities, setQuantities] = useState<Record<number, string>>({});
  const [edits, setEdits] = useState<Record<number, string>>({});
  const [payment, setPayment] = useState("success");
  const [shipping, setShipping] = useState("success");
  const [notice, setNotice] = useState("");

  async function refresh(signal?: AbortSignal) {
    const [p, c] = await Promise.all([
      api<{ products: Product[] }>("/api/products", { signal }),
      api<{ cart: Cart }>("/api/cart", { signal }),
    ]);
    if (!signal?.aborted) { setProducts(p.products); setCart(c.cart); setEdits({}); }
  }
  useEffect(() => {
    const controller = new AbortController();
    const signal = AbortSignal.any([controller.signal, AbortSignal.timeout(8000)]);
    void Promise.all([api<{ products: Product[] }>("/api/products", { signal }), api<{ cart: Cart }>("/api/cart", { signal })])
      .then(([p, c]) => { if (!signal.aborted) { setProducts(p.products); setCart(c.cart); } })
      .catch((err) => { if (!controller.signal.aborted) setError(err instanceof Error ? err : new Error("通信エラー")); });
    return () => controller.abort();
  }, []);

  async function change(productID: number, quantity: number, remove = false) {
    if (!cart || busyRef.current) return;
    busyRef.current = true; setBusy(true); setError(null); setSubmissionError(false); setNotice("");
    try {
      const result = await api<{ cart: Cart }>(`/api/cart/items/${productID}`, {
        method: remove ? "DELETE" : "PUT",
        headers: { "Content-Type": "application/json", "X-Cart-Request": "1" },
        body: JSON.stringify({ version: cart.version, ...(remove ? {} : { quantity }) }),
      });
      setCart(result.cart); setEdits({}); setNotice(remove ? "商品をカートから削除しました。" : "カートを更新しました。");
    } catch (err) {
      setError(err instanceof Error ? err : new Error("通信エラー"));
      // 更新の再送はしない。競合・通信不明時はサーバーの現在値へ合わせる。
      try { await refresh(); } catch { /* 元のエラーを表示し、手動再取得を提供する。 */ }
    } finally { busyRef.current = false; setBusy(false); }
  }
  async function checkout(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!cart || !cart.items.length || busyRef.current) return;
    busyRef.current = true; setBusy(true); setError(null); setOrder(null); setNotice(""); setSubmissionError(true);
    try {
      const result = await api<{ order: Order }>("/api/cart/checkout", {
        method: "POST", headers: { "Content-Type": "application/json", "X-Cart-Request": "1" },
        body: JSON.stringify({ version: cart.version, paymentMode: payment, shippingMode: shipping }),
      });
      setOrder(result.order); setSubmissionError(false);
      await refresh();
    } catch (err) {
      setError(err instanceof Error ? err : new Error("通信エラー"));
      try { await refresh(); } catch { /* 自動注文再送はしない。 */ }
    } finally { busyRef.current = false; setBusy(false); }
  }
  async function reload() {
    if (busyRef.current) return;
    busyRef.current = true; setBusy(true); setError(null); setSubmissionError(false);
    try { await refresh(); } catch (err) { setError(err instanceof Error ? err : new Error("通信エラー")); }
    finally { busyRef.current = false; setBusy(false); }
  }
  return <>
    <header><div className="brand">まいにち食卓<span>宅配食品</span></div><a href="#cart">カート（{cart?.items.reduce((n, i) => n + i.quantity, 0) ?? 0}点）</a><span className="badge">学習用ストア</span></header>
    <main>
      <p className="eyebrow">いつもの食卓に、ちょっといいもの。</p>
      <h1>商品一覧</h1>
      <p className="intro">商品をカートに入れて、まとめて注文できます。</p>
      <div aria-live="polite">{notice && <p role="status">{notice}</p>}</div>
      {error && <ErrorMessage error={error} submission={submissionError} />}
      {!cart && !error && <p role="status">商品とカートを読み込んでいます…</p>}
      <button className="refresh" onClick={reload} disabled={busy}>商品・カートを再取得</button>
      <ul className="products">{products.map((p) => <li className="product" key={p.id}>
        <span className="product-label">まいにちの定番</span><h2>{p.name}</h2>
        <p className="price">¥{p.priceYen.toLocaleString("ja-JP")}<span>税込</span></p>
        <p className={p.stock > 0 ? "stock" : "stock sold-out"}>{!p.stockKnown ? "在庫確認不可" : p.stock > 0 ? `在庫 ${p.stock} 点` : "在庫なし"}</p>
        <form onSubmit={(e) => { e.preventDefault(); const current = cart?.items.find((i) => i.productId === p.id)?.quantity ?? 0; void change(p.id, current + Number(quantities[p.id] ?? "1")); }}>
          <fieldset disabled={busy || !cart || !!cart.pendingOrderId}>
            <label htmlFor={`add-${p.id}`}>{p.name}の追加数量</label>
            <input id={`add-${p.id}`} type="number" min="1" max="2147483647" step="1" required value={quantities[p.id] ?? "1"} onChange={(e) => setQuantities({ ...quantities, [p.id]: e.target.value })} />
            <button type="submit">カートに追加</button>
          </fieldset>
        </form>
      </li>)}</ul>
      {cart && products.length === 0 && <p>現在、取り扱い商品はありません。</p>}
      <section id="cart" className="panel" aria-labelledby="cart-heading">
        <h2 id="cart-heading">カート</h2>
        <p className="intro">同じブラウザーで30日間保存されます。在庫は注文確定時に確認します。</p>
        {cart?.pendingOrderId && <p className="notice">処理中の注文があるためカートの変更・再注文はできません。<Link href={`/orders/${cart.pendingOrderId}`}>注文を確認・再開する</Link></p>}
        {cart?.items.length === 0 && <p>カートは空です。</p>}
        <ul className="cart-items">{cart?.items.map((item) => <li key={item.productId}>
          <div><h3>{item.productName}</h3><p>単価 ¥{item.priceYen.toLocaleString("ja-JP")} · {item.stockKnown ? `在庫 ${item.stock}点` : "在庫確認不可"}</p>
            <p>小計 ¥{item.subtotalYen.toLocaleString("ja-JP")}</p>{!cart?.pendingOrderId && item.stockKnown && item.quantity > item.stock && <p className="sold-out">在庫が不足しています。</p>}</div>
          <form onSubmit={(e) => { e.preventDefault(); void change(item.productId, Number(edits[item.productId] ?? item.quantity)); }}>
            <fieldset disabled={busy || !!cart?.pendingOrderId}>
              <label htmlFor={`cart-${item.productId}`}>{item.productName}のカート数量</label>
              <input id={`cart-${item.productId}`} type="number" min="1" max="2147483647" step="1" required value={edits[item.productId] ?? String(item.quantity)} onChange={(e) => setEdits({ ...edits, [item.productId]: e.target.value })} />
              <div className="cart-actions"><button type="submit">数量を変更</button><button type="button" onClick={() => change(item.productId, 0, true)}>削除</button></div>
            </fieldset>
          </form>
        </li>)}</ul>
        <p className="price">見積合計 ¥{(cart?.totalYen ?? 0).toLocaleString("ja-JP")}<span>税込</span></p>
        <p className="learning-note">単価は注文確定時の商品価格で決まります。数量の変更は「数量を変更」で反映してください。</p>
        <form onSubmit={checkout}><fieldset disabled={busy || !cart?.items.length || !!cart.pendingOrderId}>
          <fieldset className="mock-settings"><legend>模擬処理（注文全体・学習用）</legend>
            <p>実際の決済・配送は行いません。1商品でも在庫不足なら全体が不成立になります。</p>
            <div className="form-row"><label htmlFor="payment">模擬決済<select id="payment" value={payment} onChange={(e) => setPayment(e.target.value)}><option value="success">成功</option><option value="fail">失敗</option></select></label>
              <label htmlFor="shipping">模擬配送<select id="shipping" value={shipping} onChange={(e) => setShipping(e.target.value)}><option value="success">成功</option><option value="fail">失敗</option></select></label></div>
          </fieldset><button type="submit">{busy ? "処理中…" : "まとめて注文する"}</button>
        </fieldset></form>
        <div aria-live="polite">{order && <><OrderSummary order={order} /><Link href={`/orders/${order.id}`}>この注文の状況を確認する</Link></>}</div>
        {cart?.lastOrderId && <p><Link href={`/orders/${cart.lastOrderId}`}>このカートの最後の注文結果を確認</Link></p>}
      </section>
      <OrderLookup />
    </main><footer>まいにち食卓 · ローカル学習環境</footer>
  </>;
}
