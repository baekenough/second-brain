// @ts-check
import js from "@eslint/js";
import tseslint from "typescript-eslint";
// eslint-config-next v15+ ships flat config natively — no FlatCompat needed.
// core-web-vitals extends the base config with stricter Next.js rules.
import nextConfig from "eslint-config-next/core-web-vitals";

export default tseslint.config(
  // 1. Next.js recommended + core-web-vitals + React + a11y rules
  ...nextConfig,

  // 2. JS baseline (already included in nextConfig, but explicit for clarity)
  js.configs.recommended,

  // 3. TypeScript strict rules
  ...tseslint.configs.recommended,

  // 4. Project-specific overrides
  {
    rules: {
      "@typescript-eslint/no-explicit-any": "error",
      "@typescript-eslint/no-unused-vars": [
        "error",
        { argsIgnorePattern: "^_", varsIgnorePattern: "^_" },
      ],
      "@typescript-eslint/consistent-type-imports": [
        "error",
        { prefer: "type-imports", fixStyle: "inline-type-imports" },
      ],
      // Next.js handles img optimization — allow <img> in document detail view
      "@next/next/no-img-element": "off",
    },
  },

  // 5. Global ignores
  {
    ignores: [".next/**", "out/**", "node_modules/**", "next-env.d.ts"],
  },
);
