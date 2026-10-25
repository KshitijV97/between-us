/**
 * Enforces NFR-PERF-004: the JavaScript loaded by index.html (the initial
 * route) must stay at or below 250 KB gzipped. Lazy-loaded chunks are not
 * counted. Run after `vite build`; exits non-zero when over budget.
 */
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { gzipSync } from 'node:zlib';

const BUDGET_BYTES = 250 * 1024;
const dist = join(import.meta.dirname, '..', 'dist');
const html = readFileSync(join(dist, 'index.html'), 'utf8');

// Entry scripts and modulepreload links are what the browser fetches up front.
const urls = [...html.matchAll(/(?:src|href)="\/(assets\/[^"]+\.js)"/g)].map((m) => m[1]);
if (urls.length === 0) {
  console.error('check-bundle-size: no initial JS found in dist/index.html');
  process.exit(1);
}

let total = 0;
for (const url of new Set(urls)) {
  const size = gzipSync(readFileSync(join(dist, url)), { level: 9 }).length;
  total += size;
  console.log(`${(size / 1024).toFixed(1).padStart(8)} KB gz  ${url}`);
}
const verdict = total <= BUDGET_BYTES ? 'OK' : 'OVER BUDGET';
console.log(
  `${(total / 1024).toFixed(1).padStart(8)} KB gz  initial JS total (budget 250 KB): ${verdict}`,
);
process.exit(total <= BUDGET_BYTES ? 0 : 1);
