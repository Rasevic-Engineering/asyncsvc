import type { NextConfig } from "next";

const isDev = process.env.NODE_ENV === "development";
const apiUrl = process.env.API_URL ?? "http://localhost:8080";

// Production builds are a static export (out/) that the Go API serves from
// the same origin. In development, `next dev` proxies the API's routes so the
// browser still talks to a single origin.
const config: NextConfig = {
  output: isDev ? undefined : "export",
  images: { unoptimized: true },
  reactStrictMode: true,
  poweredByHeader: false,
  ...(isDev && {
    async rewrites() {
      return ["/v1/:path*", "/config.json", "/files/:path*", "/health", "/healthz", "/version"].map((source) => ({
        source,
        destination: `${apiUrl}${source}`,
      }));
    },
  }),
};

export default config;
