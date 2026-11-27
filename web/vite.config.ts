import { defineConfig } from "vite";

// The Go binary embeds web/dist (ADR 0001 §13).
export default defineConfig({
  build: {
    outDir: "dist",
  },
});
