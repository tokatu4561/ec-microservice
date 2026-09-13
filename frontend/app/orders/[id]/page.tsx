import Link from "next/link";
import { OrderDetails } from "../../orders";

export default async function OrderPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  return <main>
    <Link href="/">← 商品一覧に戻る</Link>
    <h1>注文状況</h1>
    <OrderDetails key={id} id={id} />
  </main>;
}
