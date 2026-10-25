/**
 * Vite build and dev-server configuration.
 *
 * In development the Go API runs separately (`make dev`), so API and probe
 * paths are proxied to it. The browser then sees a single origin, exactly as in
 * production where Go serves the embedded build or Vercel rewrites /api/*.
 */
/// <reference types="vitest/config" />
import react from '@vitejs/plugin-react';
import { defineConfig } from 'vite';

const apiTarget = process.env['CA_DEV_API_URL'] ?? 'http://127.0.0.1:7420';

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    strictPort: true,
    proxy: {
      '/api': apiTarget,
      '/healthz': apiTarget,
      '/readyz': apiTarget,
      '/version': apiTarget,
    },
  },
  build: {
    outDir: 'dist',
    // Source maps stay local: they would reveal source to anyone in hosted mode
    // and add weight to the embedded binary (NFR-PERF-001).
    sourcemap: false,
    // Vite measures minified (not gzipped) size. The real NFR-PERF-004 budget
    // (250 KB gzipped initial JS) is enforced by scripts/check-bundle-size.mjs.
    chunkSizeWarningLimit: 500,
  },
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['./src/test/setup.ts'],
    css: false,
  },
});
