import { defineConfig } from 'vite';

// The built dist/ is embedded into the Go server binary (web/embed.go)
// and served from "/". base: './' keeps asset URLs relative so the SPA
// also works when the server is fronted by a reverse proxy on a
// sub-path.
export default defineConfig({
  base: './',
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    sourcemap: false,
  },
  server: {
    proxy: {
      // Dev convenience: proxy API calls to a locally running server.
      '/healthz': 'http://localhost:8080',
      '/api': 'http://localhost:8080',
    },
  },
});
