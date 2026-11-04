#!/usr/bin/env node
// Launcher: finds the prebuilt gatt binary shipped in the matching
// platform package (installed through optionalDependencies) and runs it.
const { spawnSync } = require("child_process");
const path = require("path");

const platforms = {
  "linux-x64": "linux-amd64",
  "linux-arm64": "linux-arm64",
  "darwin-x64": "darwin-amd64",
  "darwin-arm64": "darwin-arm64",
  "win32-x64": "windows-amd64",
};
const key = `${process.platform}-${process.arch}`;
const target = platforms[key];
if (!target) {
  console.error(`gatt: no prebuilt binary for ${key}. Build from source: https://github.com/system4us/graphAllTheThings`);
  process.exit(1);
}
const pkg = `graph-all-the-things-${target}`;
let bin;
try {
  const ext = process.platform === "win32" ? ".exe" : "";
  bin = path.join(path.dirname(require.resolve(`${pkg}/package.json`)), "bin", `gatt${ext}`);
} catch {
  console.error(`gatt: platform package ${pkg} is not installed (was --no-optional / --omit=optional used?).`);
  process.exit(1);
}
const r = spawnSync(bin, process.argv.slice(2), { stdio: "inherit" });
if (r.error) { console.error(`gatt: ${r.error.message}`); process.exit(1); }
process.exit(r.status ?? (r.signal ? 1 : 0));
