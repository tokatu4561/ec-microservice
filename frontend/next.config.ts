import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  output: "standalone",
  async rewrites() {
    return [{ source: "/api/:path*", destination: "http://api:8080/api/:path*" }];
  },
};

export default nextConfig;
