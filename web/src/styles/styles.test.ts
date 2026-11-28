// @vitest-environment node
//
// design/ui/ is the source of truth for the styles. The token file is copied
// into src/styles/ without its Google Fonts @import (the CSP allows no other
// origin), so this test fails when the copy drifts from the design file.
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";

const repoRoot = resolve(import.meta.dirname, "../../..");
const designTokens = resolve(
  repoRoot,
  "design/ui/ds/smartoscbmsdesignsystemremix_a0c1fd/colors_and_type.css",
);
const copiedTokens = resolve(import.meta.dirname, "colors_and_type.css");
const appCSS = resolve(import.meta.dirname, "app.css");

describe("styles", () => {
  it("copies match design/ui", () => {
    const design = readFileSync(designTokens, "utf8");
    const copy = readFileSync(copiedTokens, "utf8");

    const copyLines = copy.split("\n");
    expect(copyLines[0]).toMatch(/^\/\* Copy of design\/ui\/.*\*\/$/);
    const copyBody = copyLines.slice(1).join("\n");

    const designLines = design.split("\n");
    const importLines = designLines.filter((line) =>
      line.startsWith('@import url("https://fonts.googleapis.com'),
    );
    expect(importLines).toHaveLength(1);
    const designBody = designLines
      .filter((line) => !line.startsWith('@import url("https://fonts.googleapis.com'))
      .join("\n");

    expect(copyBody).toBe(designBody);
    expect(copy).not.toContain("@import");
    expect(copy).not.toContain("http");
  });

  it("app.css uses the bundled font families", () => {
    const css = readFileSync(appCSS, "utf8");
    expect(css).toContain('"Inter Variable"');
    expect(css).toContain('"JetBrains Mono Variable"');
  });

  it("app.css keeps the Menu button readable on hover", () => {
    // .btn:hover in oc-glass.css outranks .oc-burger and sets a white background.
    const css = readFileSync(appCSS, "utf8");
    expect(css).toMatch(/\.oc-burger:hover\s*\{[^}]*color:\s*#FFFFFF/);
  });
});
