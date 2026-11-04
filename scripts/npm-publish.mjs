#!/usr/bin/env node
// Usage: node scripts/npm-publish.mjs <version> <distDir> [--dry-run]
// Builds the per-platform packages from release binaries in <distDir>
// (gatt-<os>-<arch>[.exe]) plus the launcher package, then publishes them.
import { mkdirSync, copyFileSync, writeFileSync, readFileSync, chmodSync, existsSync, rmSync } from "node:fs";
import { execFileSync } from "node:child_process";
import { join, resolve } from "node:path";

const [version, distArg, ...flags] = process.argv.slice(2);
if (!version || !distArg) { console.error("usage: npm-publish.mjs <version> <distDir> [--dry-run]"); process.exit(2); }
const dry = flags.includes("--dry-run");
const root = resolve(import.meta.dirname, "..");
const dist = resolve(distArg);
const out = join(root, "npm-out");
rmSync(out, { recursive: true, force: true });

const targets = [
  ["linux-amd64", "linux", "x64"], ["linux-arm64", "linux", "arm64"],
  ["darwin-amd64", "darwin", "x64"], ["darwin-arm64", "darwin", "arm64"],
  ["windows-amd64", "win32", "x64"],
];
const meta = JSON.parse(readFileSync(join(root, "npm/package.json"), "utf8"));
const publish = (dir) => execFileSync("npm", ["publish", "--access", "public", ...(dry ? ["--dry-run"] : [])], { cwd: dir, stdio: "inherit" });

const optional = {};
for (const [t, os, cpu] of targets) {
  const exe = os === "win32" ? ".exe" : "";
  const src = join(dist, `gatt-${t}${exe}`);
  if (!existsSync(src)) { console.error(`missing binary ${src}`); process.exit(1); }
  const name = `${meta.name}-${t}`;
  const dir = join(out, name);
  mkdirSync(join(dir, "bin"), { recursive: true });
  copyFileSync(src, join(dir, "bin", `gatt${exe}`));
  chmodSync(join(dir, "bin", `gatt${exe}`), 0o755);
  writeFileSync(join(dir, "package.json"), JSON.stringify({
    name, version, description: `gatt binary for ${os}-${cpu}`, license: meta.license,
    author: meta.author, repository: meta.repository, os: [os], cpu: [cpu], files: ["bin"],
  }, null, 2));
  optional[name] = version;
}

const main = join(out, meta.name);
mkdirSync(join(main, "bin"), { recursive: true });
copyFileSync(join(root, "npm/bin/gatt.js"), join(main, "bin/gatt.js"));
chmodSync(join(main, "bin/gatt.js"), 0o755);
for (const f of ["README.md", "LICENSE"]) copyFileSync(join(root, f), join(main, f));
writeFileSync(join(main, "package.json"), JSON.stringify({ ...meta, version, optionalDependencies: optional }, null, 2));

// platform packages first so the launcher's dependencies exist when it lands
for (const name of Object.keys(optional)) publish(join(out, name));
publish(main);
