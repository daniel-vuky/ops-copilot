// @vitest-environment node
//
// Negative tests for the two build-time guards: scripts/check-dist.mjs (the
// CSP and asset-cache rules on the build output) and the ESLint rules that
// keep HTML and inline styles out of the source (ADR 0002 §6).
import { spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { ESLint } from "eslint";
import { afterAll, describe, expect, it } from "vitest";

const webDir = resolve(import.meta.dirname, "../..");
const checkDist = join(webDir, "scripts/check-dist.mjs");
const fixtureRoot = mkdtempSync(join(tmpdir(), "oc-check-dist-"));

afterAll(() => {
  rmSync(fixtureRoot, { recursive: true, force: true });
});

const cleanIndex = `<!doctype html>
<html lang="en">
  <head>
    <meta charset="UTF-8" />
    <link rel="icon" type="image/svg+xml" href="/favicon.svg" />
    <script type="module" crossorigin src="/assets/index-AbCd1234.js"></script>
    <link rel="stylesheet" crossorigin href="/assets/index-Xy_z-987.css">
  </head>
  <body>
    <div id="root"></div>
  </body>
</html>
`;

const cleanCSS =
  '@font-face{font-family:"Inter Variable";src:url(/assets/inter-latin-wght-normal-Q1w2E3r4.woff2) format("woff2-variations")}';

/** The files of a build that passes every check. */
function cleanBuild(): Record<string, string> {
  return {
    "index.html": cleanIndex,
    "favicon.svg": '<svg xmlns="http://www.w3.org/2000/svg"></svg>',
    "assets/index-AbCd1234.js": 'console.log("ok");',
    "assets/index-Xy_z-987.css": cleanCSS,
    "assets/inter-latin-wght-normal-Q1w2E3r4.woff2": "wOF2",
  };
}

let fixtureCount = 0;

/** Writes files into a new fixture directory and runs check-dist on it. */
function runCheckDist(files: Record<string, string>) {
  fixtureCount += 1;
  const dir = join(fixtureRoot, `case-${fixtureCount}`);
  for (const [name, content] of Object.entries(files)) {
    const path = join(dir, name);
    mkdirSync(dirname(path), { recursive: true });
    writeFileSync(path, content);
  }
  return spawnSync(process.execPath, [checkDist, dir], {
    cwd: webDir,
    encoding: "utf8",
  });
}

describe("check-dist", () => {
  it("accepts a clean build", () => {
    const result = runCheckDist(cleanBuild());
    expect(result.stderr).toBe("");
    expect(result.status).toBe(0);
    expect(result.stdout).toContain("check-dist: ok (5 files)");
  });

  const broken: [string, (files: Record<string, string>) => void, string][] = [
    [
      "an inline <script>",
      (f) => {
        f["index.html"] = cleanIndex.replace(
          '<div id="root"></div>',
          '<div id="root"></div><script>alert(1)</script>',
        );
      },
      "inline <script>",
    ],
    [
      "a style= attribute",
      (f) => {
        f["index.html"] = cleanIndex.replace(
          '<div id="root">',
          '<div id="root" style="color:red">',
        );
      },
      "style= attribute",
    ],
    [
      "a Google Fonts URL in CSS",
      (f) => {
        f["assets/index-Xy_z-987.css"] =
          '@import url("https://fonts.googleapis.com/css2?family=Inter");' +
          cleanCSS;
      },
      "Google Fonts reference",
    ],
    [
      "a url(data:) font",
      (f) => {
        f["assets/index-Xy_z-987.css"] =
          '@font-face{font-family:x;src:url(data:font/woff2;base64,AAAA)}';
      },
      "url(data:)",
    ],
    [
      "an unhashed asset",
      (f) => {
        f["assets/inter-latin-wght-normal.woff2"] = "wOF2";
      },
      "assets/inter-latin-wght-normal.woff2: asset name has no content hash",
    ],
  ];

  it.each(broken)("rejects %s", (_name, breakIt, problem) => {
    const files = cleanBuild();
    breakIt(files);
    const result = runCheckDist(files);
    expect(result.status).toBe(1);
    expect(result.stderr).toContain(problem);
    expect(result.stdout).not.toContain("check-dist: ok");
  });
});

describe("ESLint guards", () => {
  const eslint = new ESLint({ cwd: webDir });

  /** Lints a snippet as src/x.tsx and returns the rule ids it reports. */
  async function ruleIds(code: string): Promise<(string | null)[]> {
    const [result] = await eslint.lintText(code, { filePath: "src/x.tsx" });
    return result.messages.map((m) => m.ruleId);
  }

  const forbidden: [string, string][] = [
    [
      "dangerouslySetInnerHTML as a JSX attribute",
      "export const A = (s: string) => <div dangerouslySetInnerHTML={{ __html: s }} />;\n",
    ],
    [
      "dangerouslySetInnerHTML in a spread object",
      'export const A = (x: { __html: string }) => <div {...{ "dangerouslySetInnerHTML": x }} />;\n',
    ],
    [
      "a style attribute",
      'export const A = () => <p style={{ color: "red" }}>x</p>;\n',
    ],
    [
      "an innerHTML assignment",
      "export function f(el: HTMLElement, s: string) {\n  el.innerHTML = s;\n}\n",
    ],
    [
      "an insertAdjacentHTML call",
      'export function f(el: HTMLElement, s: string) {\n  el.insertAdjacentHTML("beforeend", s);\n}\n',
    ],
  ];

  it.each(forbidden)("reports %s", async (_name, code) => {
    expect(await ruleIds(code)).toContain("no-restricted-syntax");
  });

  it("accepts text rendered as text", async () => {
    const code =
      'export const A = ({ s }: { s: string }) => <p className="x">{s}</p>;\n';
    expect(await ruleIds(code)).toEqual([]);
  });
});
