import { defineConfig } from 'astro/config';
import svelte from '@astrojs/svelte';
import mdx from '@astrojs/mdx';
import tailwindcss from '@tailwindcss/vite';
import rehypeExternalLinks from 'rehype-external-links';
import rehypeCopyButton from './src/lib/rehype-copy-button.mjs';
import rehypeSectionWrapper from './src/lib/rehype-section-wrapper.mjs';
import { fileURLToPath } from 'node:url';

export default defineConfig({
  integrations: [mdx(), svelte()],
  // Open every external link in a new tab and append a small arrow
  // so readers can see which links leave the docs site before they
  // click. Internal links (relative paths, anchors) are left alone.
  markdown: {
    rehypePlugins: [
      [
        rehypeExternalLinks,
        {
          target: '_blank',
          rel: ['noopener', 'noreferrer'],
          content: { type: 'text', value: ' ↗' }, // ↗
        },
      ],
      // Wrap fenced code blocks with a "Copy" button. Inherited by MDX via
      // @astrojs/mdx's default extendMarkdownConfig.
      rehypeCopyButton,
      // Wrap each h2 + following siblings in a <section class="prose-section">
      // so the docs render as card-stacked sections.
      rehypeSectionWrapper,
    ],
  },
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
