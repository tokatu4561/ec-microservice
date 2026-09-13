import type { Metadata } from "next";
import "./globals.css";

export const metadata: Metadata = {
  title: "まいにち食卓 | 商品一覧",
  description: "宅配食品ECの学習用アプリ",
};

export default function RootLayout({ children }: Readonly<{ children: React.ReactNode }>) {
  return <html lang="ja"><body>{children}</body></html>;
}
