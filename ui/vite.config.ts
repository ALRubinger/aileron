import { sveltekit } from '@sveltejs/kit/vite';
import tailwindcss from '@tailwindcss/vite';
import { defineConfig } from 'vitest/config';

export default defineConfig({
	plugins: [tailwindcss(), sveltekit()],
	test: {
		environment: 'jsdom',
		include: ['src/**/*.test.ts'],
		globals: true,
		setupFiles: ['src/tests/setup.ts'],
		restoreMocks: true,
		alias: {
			'$env/static/public': new URL('./src/tests/mocks/env.ts', import.meta.url).pathname
		},
		// Resolve client-side Svelte bundle (not SSR) so mount() works in jsdom
		server: {
			deps: {
				inline: [/bits-ui/]
			}
		}
	},
	resolve: {
		conditions: ['browser']
	}
});
