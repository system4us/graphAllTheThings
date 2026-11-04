// postinstall: shrink node_modules for a CPU-only worker.
//
// onnxruntime-node ships native binaries for every OS/arch plus GPU execution
// providers (CUDA alone is ~270 MB). gatt-embed runs on CPU for the current
// platform only, so everything else is dead weight (~740 MB -> ~170 MB).
// onnxruntime-web (the browser/WASM runtime, ~140 MB) is also unused: in Node,
// transformers.js loads onnxruntime-node.
// GATT_EMBED_KEEP_GPU=1 keeps the GPU provider libraries; GATT_EMBED_KEEP_WEB=1
// keeps onnxruntime-web.
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const root = path.join(path.dirname(fileURLToPath(import.meta.url)), 'node_modules', 'onnxruntime-node', 'bin');
const keepGpu = process.env.GATT_EMBED_KEEP_GPU === '1';
const gpuLib = /providers_(cuda|tensorrt|rocm|migraphx|openvino|dnnl)/i;

let freed = 0;
const size = (p) => {
  const st = fs.lstatSync(p);
  if (!st.isDirectory()) return st.size;
  return fs.readdirSync(p).reduce((n, f) => n + size(path.join(p, f)), 0);
};
const rm = (p) => { freed += size(p); fs.rmSync(p, { recursive: true, force: true }); };

if (fs.existsSync(root)) {
  for (const napi of fs.readdirSync(root)) {
    const napiDir = path.join(root, napi);
    for (const plat of fs.readdirSync(napiDir)) {
      const platDir = path.join(napiDir, plat);
      if (plat !== process.platform) { rm(platDir); continue; }
      for (const arch of fs.readdirSync(platDir)) {
        const archDir = path.join(platDir, arch);
        if (arch !== process.arch) { rm(archDir); continue; }
        if (keepGpu) continue;
        for (const f of fs.readdirSync(archDir)) if (gpuLib.test(f)) rm(path.join(archDir, f));
      }
    }
  }
}
if (process.env.GATT_EMBED_KEEP_WEB !== '1') {
  const web = path.join(path.dirname(root), '..', 'onnxruntime-web');
  if (fs.existsSync(web)) rm(web);
}
console.log(`gatt-embed: pruned ${(freed / 1e6).toFixed(0)} MB of unused onnxruntime binaries`);
