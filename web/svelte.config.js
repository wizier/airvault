import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';

export default {
  // Enables <script lang="ts"> and PostCSS-less scoped styles.
  preprocess: vitePreprocess(),
};
