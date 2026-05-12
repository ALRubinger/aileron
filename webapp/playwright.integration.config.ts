import { defineConfig, devices } from '@playwright/test';

// Real-daemon integration config. The daemon is expected to be running
// already, started with `--dev-seed-approvals=test/fixtures/action-approvals/basic.json`
// and its bound URL exposed via the `AILERON_WEBAPP_URL` env var. The
// repo's `task test:webapp:e2e:integration` Taskfile target wires
// everything up; see `webapp/scripts/run-integration-e2e.sh` for the
// startup/teardown sequence.
//
// Tests run sequentially with one worker because the daemon's queue is
// shared mutable state — each test approves or denies from a common
// initial seed.
export default defineConfig({
	testDir: 'e2e-integration',
	fullyParallel: false,
	forbidOnly: !!process.env.CI,
	retries: process.env.CI ? 2 : 0,
	workers: 1,
	reporter: process.env.CI
		? [
				['github'],
				[
					'junit',
					{ outputFile: 'test-results/junit-playwright-integration.xml' }
				]
			]
		: 'html',
	use: {
		baseURL: process.env.AILERON_WEBAPP_URL,
		trace: 'on-first-retry'
	},
	projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }]
});
