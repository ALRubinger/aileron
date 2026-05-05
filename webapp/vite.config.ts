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
		reporters: ['default', 'junit'],
		outputFile: { junit: 'test-results/junit-vitest.xml' },
		coverage: {
			provider: 'v8',
			reporter: ['text', 'lcov'],
			reportsDirectory: 'coverage',
			include: ['src/lib/**', 'src/routes/**'],
			exclude: ['src/tests/**']
		},
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
