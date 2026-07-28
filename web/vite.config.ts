import { defineConfig } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';
import tailwindcss from '@tailwindcss/vite';

// AirVault SPA build. Output lands in `dist/` (Vite default), which the Go
// daemon embeds and serves as static files. Routing is hash-based.
export default defineConfig({
  plugins: [tailwindcss(), svelte()],
  server: {
    host: '127.0.0.1',
    port: 8080,
    strictPort: true,
    proxy: {
      '/api': 'http://127.0.0.1:8081',
      '/healthz': 'http://127.0.0.1:8081',
    },
  },
});
