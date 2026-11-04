#!/usr/bin/env node
// Usage: node scripts/npm-publish.mjs <version> [--dry-run]
// Publishes the single launcher package. The binaries are NOT bundled: the
// package downloads them from the GitHub release v<version> on install.
import { mkdirSync, cpSync, copyFileSync, writeFileSync, readFileSync, rmSync } from "node:fs";
import { execFileSync } from "node:child_process";
import { join, resolve } from "node:path";

const [version, ...flags] = process.argv.slice(2);
if (!version) { console.error("usage: npm-publish.mjs <version> [--dry-run]"); process.exit(2); }
const root = resolve(import.meta.dirname, "..");
const out = join(root, "npm-out");
rmSync(out, { recursive: true, force: true });
mkdirSync(out, { recursive: true });
cpSync(join(root, "npm/bin"), join(out, "bin"), { recursive: true });
copyFileSync(join(root, "npm/install.js"), join(out, "install.js"));
for (const f of ["README.md", "LICENSE"]) copyFileSync(join(root, f), join(out, f));
const meta = JSON.parse(readFileSync(join(root, "npm/package.json"), "utf8"));
writeFileSync(join(out, "package.json"), JSON.stringify({ ...meta, version }, null, 2));
execFileSync("npm", ["publish", "--access", "public", ...(flags.includes("--dry-run") ? ["--dry-run"] : [])], { cwd: out, stdio: "inherit" });
