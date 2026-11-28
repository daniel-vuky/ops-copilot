import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

// The Go binary embeds web/dist (ADR 0001 §13). Vite writes into dist/app so
// the tracked dist/.gitkeep survives emptyOutDir and go:embed always has a
// file to match, even before the first frontend build.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: "dist/app",
    emptyOutDir: true,
    // Fonts and images stay separate hashed files: CSP allows no data: fonts.
    assetsInlineLimit: 0,
    // The polyfill would be an inline script, which the CSP blocks.
    modulePreload: { polyfill: false },
    sourcemap: false,
  },
  test: {
    environment: "jsdom",
    setupFiles: ["./src/test/setup.ts"],
    include: ["src/**/*.test.{ts,tsx}"],
  },
});
