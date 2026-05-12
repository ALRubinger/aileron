import { defineConfig, devices } from '@playwright/test';

// Mocked-API E2E config. The webServer runs `pnpm preview` against the
// already-built static output in `build/`; every `/v1/*` request the
// page makes is intercepted by `page.route()` inside the specs. Pair
// with `playwright.integration.config.ts` for the real-daemon tier.
//
// Run `task build:webapp` (or `pnpm build` inside this directory) before
// invoking Playwright — `pnpm preview` will 404 without a fresh build.
export default defineConfig({
	testDir: 'e2e',
	fullyParallel: true,
	forbidOnly: !!process.env.CI,
	retries: process.env.CI ? 2 : 0,
	workers: process.env.CI ? 1 : undefined,
	reporter: process.env.CI
		? [['github'], ['junit', { outputFile: 'test-results/junit-playwright.xml' }]]
		: 'html',
	use: {
		baseURL: 'http://localhost:4173',
		trace: 'on-first-retry'
	},
	projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
	webServer: {
		command: 'pnpm preview --port 4173 --strictPort',
		port: 4173,
		reuseExistingServer: !process.env.CI,
		stdout: 'pipe',
		stderr: 'pipe'
	}
});
