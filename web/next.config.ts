import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  output: "standalone",
  // Disable X-Powered-By header
  poweredByHeader: false,
  // Compiler options
  compiler: {
    // Remove console.log in production
    removeConsole:
      process.env.NODE_ENV === "production"
        ? { exclude: ["error", "warn"] }
        : false,
  },
};

export default nextConfig;
