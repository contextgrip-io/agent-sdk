import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// The Go API server. In production the built UI is embedded into that binary
// and served same-origin, so all API calls use relative URLs; in dev we proxy
// them to a locally running server instead.
const backend = 'http://localhost:8080';

export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      '/api': backend,
      '/healthz': backend,
      '/readyz': backend,
    },
  },
  build: {
    outDir: 'dist',
  },
});
