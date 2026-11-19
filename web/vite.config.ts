import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/api': 'http://127.0.0.1:7788',
      '/health': 'http://127.0.0.1:7788',
      '/status': 'http://127.0.0.1:7788',
      '/repos': 'http://127.0.0.1:7788',
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
});
