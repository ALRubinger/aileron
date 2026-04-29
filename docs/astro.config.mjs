import { defineConfig } from 'astro/config';
import svelte from '@astrojs/svelte';
import mdx from '@astrojs/mdx';
import tailwindcss from '@tailwindcss/vite';
import { fileURLToPath } from 'node:url';

export default defineConfig({
  integrations: [mdx(), svelte()],
  vite: {
    resolve: {
      alias: {
        $lib: fileURLToPath(new URL('./src/lib', import.meta.url))
      }
    },
    plugins: [tailwindcss()]
  },
  output: 'static'
});
