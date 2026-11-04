#!/usr/bin/env node
// Launcher: runs the gatt binary, downloading it from the matching GitHub
// release (and verifying its SHA-256) on first use if postinstall didn't.
const { spawnSync } = require("child_process");
const { install, binPath } = require("../install.js");

(async () => {
  let bin;
  try {
    bin = await install();
  } catch (e) {
    console.error(`gatt: ${e.message}`);
    process.exit(1);
  }
  const r = spawnSync(bin, process.argv.slice(2), { stdio: "inherit" });
  if (r.error) { console.error(`gatt: ${r.error.message}`); process.exit(1); }
  process.exit(r.status ?? (r.signal ? 1 : 0));
})();
