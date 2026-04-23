import { sveltekit } from '@sveltejs/kit/vite';
import tailwindcss from '@tailwindcss/vite';
import istanbul from 'vite-plugin-istanbul';
import { defineConfig } from 'vitest/config';

const e2eCoverage = !!process.env.E2E_COVERAGE;

export default defineConfig({
	plugins: [
		tailwindcss(),
		sveltekit(),
		...(e2eCoverage
			? [
					istanbul({
						include: 'src/**/*',
						exclude: ['node_modules', 'src/tests/**'],
						extension: ['.ts', '.svelte'],
						forceBuildInstrument: true
					})
				]
			: [])
	],
	test: {
		environment: 'jsdom',
		include: ['src/**/*.test.ts'],
		globals: true,
		setupFiles: ['src/tests/setup.ts'],
		restoreMocks: true,
		reporters: ['default', 'junit'],
		outputFile: { junit: 'test-results/junit-vitest.xml' },
		coverage: {
			provider: 'v8',
			reporter: ['text', 'lcov'],
			reportsDirectory: 'coverage',
			include: ['src/lib/**', 'src/routes/**'],
			exclude: ['src/tests/**']
		},
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
