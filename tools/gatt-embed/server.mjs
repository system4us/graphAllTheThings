// gatt-embed: local ONNX embedding worker.
//
//   node server.mjs [--port 0] [--model Xenova/paraphrase-multilingual-MiniLM-L12-v2]
//
// Serves an OpenAI-compatible POST /v1/embeddings (what `gatt --embed-url .../v1`
// speaks) and GET /health. The model is downloaded on first use into the
// transformers.js cache and runs on CPU via ONNX Runtime. Loopback only.
import http from 'node:http';
import { pipeline, env } from '@huggingface/transformers';

const args = process.argv.slice(2);
const opt = (name, dflt) => {
  const i = args.indexOf(`--${name}`);
  return i >= 0 && args[i + 1] !== undefined ? args[i + 1] : dflt;
};
const MODEL = opt('model', process.env.GATT_EMBED_MODEL || 'Xenova/paraphrase-multilingual-MiniLM-L12-v2');
const PORT = Number(opt('port', process.env.GATT_EMBED_PORT || 0));
const MAX_BODY = 8 * 1024 * 1024;
const MAX_BATCH = 256;
if (process.env.GATT_EMBED_CACHE) env.cacheDir = process.env.GATT_EMBED_CACHE;

let extractor = null; // loaded lazily so /health answers while the model downloads
let loading = null;
let loadError = null;
const load = () => (loading ??= pipeline('feature-extraction', MODEL, { dtype: 'q8' })
  .then((p) => { extractor = p; })
  .catch((e) => { loadError = String(e?.message || e); loading = null; throw e; }));

const loopback = (host) => ['127.0.0.1', 'localhost', '::1', '[::1]'].includes(host);
const json = (res, code, body) => {
  res.writeHead(code, { 'content-type': 'application/json' });
  res.end(JSON.stringify(body));
};
const readBody = (req) => new Promise((resolve, reject) => {
  let size = 0; const chunks = [];
  req.on('data', (c) => { size += c.length; if (size > MAX_BODY) { reject(new Error('body too large')); req.destroy(); } else chunks.push(c); });
  req.on('end', () => resolve(Buffer.concat(chunks).toString('utf8')));
  req.on('error', reject);
});

const server = http.createServer(async (req, res) => {
  // Same posture as `gatt serve`: refuse DNS rebinding and browser-originated requests.
  const host = (req.headers.host || '').replace(/:\d+$/, '');
  if (!loopback(host) || req.headers.origin) return json(res, 403, { error: 'forbidden' });

  if (req.method === 'GET' && req.url === '/health') {
    return json(res, 200, { ok: true, model: MODEL, ready: extractor !== null, error: loadError });
  }
  if (req.method === 'POST' && req.url === '/v1/embeddings') {
    try {
      const body = JSON.parse(await readBody(req) || '{}');
      const input = Array.isArray(body.input) ? body.input : [body.input];
      if (!input.length || input.length > MAX_BATCH || input.some((s) => typeof s !== 'string')) {
        return json(res, 400, { error: { message: `input must be 1..${MAX_BATCH} strings` } });
      }
      await load();
      const out = await extractor(input, { pooling: 'mean', normalize: true });
      const [n, dim] = out.dims;
      const data = [];
      for (let i = 0; i < n; i++) {
        data.push({ object: 'embedding', index: i, embedding: Array.from(out.data.subarray(i * dim, (i + 1) * dim)) });
      }
      return json(res, 200, { object: 'list', model: MODEL, data });
    } catch (e) {
      return json(res, 500, { error: { message: String(e?.message || e) } });
    }
  }
  json(res, 404, { error: 'not found' });
});

server.listen(PORT, '127.0.0.1', () => {
  // The parent (`gatt serve --onnx`) parses this line to learn the port.
  console.log(`gatt-embed listening on http://127.0.0.1:${server.address().port} model=${MODEL}`);
  load().catch((e) => console.error('model load failed:', e?.message || e));
});

for (const sig of ['SIGINT', 'SIGTERM']) process.on(sig, () => server.close(() => process.exit(0)));
