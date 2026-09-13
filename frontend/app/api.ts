export type Product = { id: number; name: string; priceYen: number; stock: number };
export type OrderItem = { productId: number; productName: string; quantity: number; priceYen: number; subtotalYen: number; stockShortage: boolean };
export type Order = {
  id: string; items: OrderItem[]; totalYen: number;
  status: "shipping_requested" | "failed";
  failureReason: "out_of_stock" | "payment_failed" | "shipping_failed" | null;
  paymentStatus: "not_started" | "succeeded" | "failed" | "cancelled";
  shippingStatus: "not_started" | "requested" | "failed";
  createdAt: string;
};
export type CartItem = { productId: number; productName: string; quantity: number; priceYen: number; stock: number; subtotalYen: number };
export type Cart = { version: number; items: CartItem[]; totalYen: number; expiresAt: string; lastOrderId: string | null };

export class ApiError extends Error {
  constructor(message: string, public status: number, public requestId?: string, public orderId?: string) { super(message); }
}

export async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, { ...init, cache: "no-store", signal: init?.signal ?? AbortSignal.timeout(8000) });
  if (!response.ok) {
    const body = await response.json().catch(() => ({}));
    throw new ApiError(body.error ?? "サーバーに接続できませんでした。", response.status,
      response.headers.get("X-Request-ID") ?? undefined, body.orderId);
  }
  return response.json();
}
