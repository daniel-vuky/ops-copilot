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

/**
 * Lays out a fixture web/ directory with the clean build in dist/app plus
 * files (paths relative to dist/), then runs check-dist from it with no
 * argument, so the default dist/app and its parent are checked.
 */
function runCheckDistDefault(distFiles: Record<string, string>) {
  fixtureCount += 1;
  const web = join(fixtureRoot, `case-${fixtureCount}`);
  const files: Record<string, string> = { ...distFiles };
  for (const [name, content] of Object.entries(cleanBuild())) {
    files[`app/${name}`] = content;
  }
  for (const [name, content] of Object.entries(files)) {
    const path = join(web, "dist", name);
    mkdirSync(dirname(path), { recursive: true });
    writeFileSync(path, content);
  }
  return spawnSync(process.execPath, [checkDist], {
    cwd: web,
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
    [
      "an https: URL in CSS url()",
      (f) => {
        f["assets/index-Xy_z-987.css"] =
          "@font-face{font-family:x;src:url(https://cdn.example/x.woff2)}";
      },
      "assets/index-Xy_z-987.css: URL to another origin",
    ],
    [
      "a protocol-relative URL in CSS url()",
      (f) => {
        f["assets/index-Xy_z-987.css"] =
          "@font-face{font-family:x;src:url(//cdn.example/x.woff2)}";
      },
      "assets/index-Xy_z-987.css: URL to another origin",
    ],
    [
      "a minified @import of another origin",
      (f) => {
        f["assets/index-Xy_z-987.css"] =
          '@import"https://cdn.example/x.css";' + cleanCSS;
      },
      "assets/index-Xy_z-987.css: URL to another origin",
    ],
    [
      "a <style> element",
      (f) => {
        f["index.html"] = cleanIndex.replace(
          "</head>",
          "<style>p{color:red}</style></head>",
        );
      },
      "index.html: <style> element",
    ],
    [
      "a <base> element",
      (f) => {
        f["index.html"] = cleanIndex.replace("</head>", '<base href="/"></head>');
      },
      "index.html: <base> element",
    ],
    [
      "a <meta http-equiv>",
      (f) => {
        f["index.html"] = cleanIndex.replace(
          "</head>",
          '<meta http-equiv="refresh" content="0;url=/x"></head>',
        );
      },
      "index.html: <meta http-equiv>",
    ],
    [
      "an inline <script> with only data-src",
      (f) => {
        f["index.html"] = cleanIndex.replace(
          '<div id="root"></div>',
          '<div id="root"></div><script data-src="x">alert(1)</script>',
        );
      },
      "index.html: inline <script>",
    ],
    [
      "a missing index.html",
      (f) => {
        delete f["index.html"];
      },
      "index.html: missing",
    ],
    [
      "a dotfile in assets",
      (f) => {
        f["assets/.DS_Store"] = "x";
      },
      'assets/.DS_Store: name starts with "."',
    ],
    [
      "a public file in a sub-directory",
      (f) => {
        f["img/logo.png"] = "PNG";
      },
      "img/logo.png: not servable (only root files and assets/ are served)",
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

  // go:embed takes all of web/dist, so with the default directory nothing but
  // .gitkeep and app/ may sit beside the build.
  it("accepts the default dist with only .gitkeep and app/", () => {
    const result = runCheckDistDefault({ ".gitkeep": "" });
    expect(result.stderr).toBe("");
    expect(result.status).toBe(0);
    expect(result.stdout).toContain("check-dist: ok (5 files)");
  });

  it.each([
    ["a stale file", { ".gitkeep": "", "index-old.js": "x" }, "dist/index-old.js"],
    ["a dotfile", { ".gitkeep": "", ".DS_Store": "x" }, "dist/.DS_Store"],
    ["a stray directory", { ".gitkeep": "", "old/index.html": "x" }, "dist/old"],
  ])("rejects %s beside the default dist/app", (_name, files, problem) => {
    const result = runCheckDistDefault(files);
    expect(result.status).toBe(1);
    expect(result.stderr).toContain(`check-dist: ${problem}:`);
    expect(result.stdout).not.toContain("check-dist: ok");
  });
});

describe("ESLint guards", () => {
  const eslint = new ESLint({ cwd: webDir });

  const asText = "Render text as text (ADR 0002 §6)";
  const noStyle = "Use a class from app.css; no inline styles (CSP)";

  /** Lints a snippet as src/x.tsx and returns `<ruleId>: <message>` lines. */
  async function report(code: string): Promise<string[]> {
    const [result] = await eslint.lintText(code, { filePath: "src/x.tsx" });
    return result.messages.map((m) => `${m.ruleId}: ${m.message}`);
  }

  /** Wraps a statement in a function that has an element and a string. */
  const inFn = (statement: string) =>
    `export function f(el: HTMLElement, s: string) {\n  ${statement}\n}\n`;

  const forbidden: [string, string, string][] = [
    [
      "dangerouslySetInnerHTML as a JSX attribute",
      "export const A = (s: string) => <div dangerouslySetInnerHTML={{ __html: s }} />;\n",
      asText,
    ],
    [
      "dangerouslySetInnerHTML in a spread object",
      'export const A = (x: { __html: string }) => <div {...{ "dangerouslySetInnerHTML": x }} />;\n',
      asText,
    ],
    [
      "a style attribute",
      'export const A = () => <p style={{ color: "red" }}>x</p>;\n',
      noStyle,
    ],
    ["an innerHTML assignment", inFn("el.innerHTML = s;"), asText],
    ["a computed innerHTML assignment", inFn('el["innerHTML"] = s;'), asText],
    ["a computed outerHTML assignment", inFn('el["outerHTML"] = s;'), asText],
    [
      "an insertAdjacentHTML call",
      inFn('el.insertAdjacentHTML("beforeend", s);'),
      asText,
    ],
    ["a setAttribute(\"style\") call", inFn('el.setAttribute("style", s);'), noStyle],
    ["a document.write call", inFn("document.write(s);"), asText],
    ["a document.writeln call", inFn("document.writeln(s);"), asText],
    [
      "a srcDoc attribute",
      'export const A = (s: string) => <iframe title="x" srcDoc={s} />;\n',
      asText,
    ],
  ];

  it.each(forbidden)("reports %s", async (_name, code, message) => {
    expect(await report(code)).toContain(`no-restricted-syntax: ${message}`);
  });

  // The guards cover plain JavaScript under src/ too, so a .jsx file cannot
  // slip past them.
  it("reports dangerouslySetInnerHTML in a .jsx file", async () => {
    const [result] = await eslint.lintText(
      "export const A = (s) => <div dangerouslySetInnerHTML={{ __html: s }} />;\n",
      { filePath: "src/x.jsx" },
    );
    expect(result.messages.map((m) => `${m.ruleId}: ${m.message}`)).toContain(
      `no-restricted-syntax: ${asText}`,
    );
  });

  it("accepts text rendered as text", async () => {
    const code =
      'export const A = ({ s }: { s: string }) => <p className="x">{s}</p>;\n';
    expect(await report(code)).toEqual([]);
  });

  it("accepts a setAttribute call for another attribute", async () => {
    expect(await report(inFn('el.setAttribute("title", s);'))).toEqual([]);
  });
});
