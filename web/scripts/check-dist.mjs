// Checks the Vite build output before the Go binary embeds it (ADR 0001 §13).
//
// The CSP is `default-src 'self'; img-src 'self' data:; ...`, so the build
// must hold no inline script or style, no other origin and no data: fonts,
// and every file under assets/ must be content-hashed, because the server
// caches assets/ as immutable for a year.
//
// Usage: node scripts/check-dist.mjs [dir]   (default: dist/app, from web/)
// With the default directory it also checks dist/ itself: go:embed takes all
// of web/dist, so nothing but .gitkeep and app/ may sit there.
// Exits 1 with one line per problem, or prints `check-dist: ok (<n> files)`.
// Node standard library only.

import { readdirSync, readFileSync, statSync } from "node:fs";
import { dirname, join, relative } from "node:path";

const defaultDir = "dist/app";
const dir = process.argv[2] ?? defaultDir;

// Paths whose name starts with "." anywhere in the build. The server answers
// 404 for them, so a dotfile in the build is dead weight or a leak
// (.DS_Store, .env); they are reported and not descended into.
const dotted = [];

/** Lists every regular file under root, recursively, minus dotted names. */
function listFiles(root) {
  const files = [];
  for (const entry of readdirSync(root, { withFileTypes: true })) {
    const path = join(root, entry.name);
    if (entry.name.startsWith(".")) {
      dotted.push(path);
    } else if (entry.isDirectory()) {
      files.push(...listFiles(path));
    } else if (entry.isFile()) {
      files.push(path);
    }
  }
  return files;
}

// A remote URL: http:, https: or protocol-relative //, inside url(), href=,
// src= or @import (minified CSS drops the space: @import"https://...").
const remoteURL =
  /(?:url\(\s*['"]?|href\s*=\s*['"]?|src\s*=\s*['"]?|@import\s*(?:url\(\s*)?['"]?)\s*(?:https?:|\/\/)/i;

// Inline or page-level constructs the CSP blocks or that change how URLs
// and policies resolve.
const htmlRules = [
  [/<script\b(?![^>]*\ssrc\s*=)[^>]*>/i, "inline <script> (no src=)"],
  [/<style\b/i, "<style> element"],
  [/\sstyle\s*=/i, "style= attribute"],
  [/<base\b/i, "<base> element"],
  [/<meta\b[^>]*\bhttp-equiv\b/i, "<meta http-equiv>"],
];

// Vite 8 names hashed files `<name>-<8 chars>.<ext>`. This is a heuristic: a
// source file whose own name ends in an 8-character segment would pass too.
const hashed = /-[A-Za-z0-9_-]{8}\.[a-z0-9]+$/;

const problems = [];
let files;
try {
  if (!statSync(dir).isDirectory()) {
    throw new Error("not a directory");
  }
  files = listFiles(dir);
} catch (err) {
  console.error(`check-dist: cannot read ${dir}: ${err.message}`);
  process.exit(1);
}

// The binary embeds web/dist with `all:`, dotfiles included, so a stale or
// stray entry beside the build would ship in it. Fixture runs pass a
// directory and skip this.
if (process.argv[2] === undefined) {
  const parent = dirname(defaultDir);
  for (const entry of readdirSync(parent, { withFileTypes: true })) {
    const allowed =
      (entry.name === ".gitkeep" && entry.isFile()) ||
      (entry.name === "app" && entry.isDirectory());
    if (!allowed) {
      problems.push(
        `${parent}/${entry.name}: only .gitkeep and app/ may sit in ${parent}/`,
      );
    }
  }
}

for (const path of dotted) {
  const rel = relative(dir, path).split("\\").join("/");
  problems.push(`${rel}: name starts with "."`);
}

const indexPath = join(dir, "index.html");
if (!files.includes(indexPath)) {
  problems.push("index.html: missing");
}

for (const file of files) {
  const rel = relative(dir, file).split("\\").join("/");
  const text = readFileSync(file, "latin1");

  if (rel === "index.html") {
    for (const [pattern, what] of htmlRules) {
      if (pattern.test(text)) {
        problems.push(`${rel}: ${what}`);
      }
    }
  }
  if (rel === "index.html" || rel.endsWith(".css")) {
    if (remoteURL.test(text)) {
      problems.push(`${rel}: URL to another origin`);
    }
  }
  if (rel.endsWith(".css") && /url\(\s*['"]?\s*data:/i.test(text)) {
    problems.push(`${rel}: url(data:) (inline font or image)`);
  }
  if (/fonts\.(?:googleapis|gstatic)\.com/i.test(text)) {
    problems.push(`${rel}: Google Fonts reference`);
  }
  if (rel.startsWith("assets/") && !hashed.test(rel)) {
    problems.push(`${rel}: asset name has no content hash`);
  }
}

if (problems.length > 0) {
  for (const problem of problems) {
    console.error(`check-dist: ${problem}`);
  }
  process.exit(1);
}

console.log(`check-dist: ok (${files.length} files)`);
