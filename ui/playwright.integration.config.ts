import { defineConfig, devices } from '@playwright/test';

/**
 * Playwright config for integration E2E tests that run against the
 * real Docker Compose stack (server + UI + database).
 *
 * Prerequisites:
 *   task up -- -d --build --wait
 *   # or: docker compose -f deploy/docker-compose.yml up -d --build --wait
 *
 * Usage:
 *   pnpm test:e2e:integration
 */
export default defineConfig({
	testDir: 'e2e-integration',
	fullyParallel: false, // sequential — tests share a single user account
	forbidOnly: !!process.env.CI,
	retries: process.env.CI ? 2 : 0,
	workers: 1, // single worker — shared auth state
	reporter: process.env.CI
		? [['github'], ['junit', { outputFile: 'test-results/junit-playwright-integration.xml' }]]
		: 'html',
	use: {
		baseURL: process.env.AILERON_UI_BASE_URL || 'http://localhost:3000',
		trace: 'on-first-retry'
	},
	projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }]
	// No webServer — expects the Docker stack to already be running
});
