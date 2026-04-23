import { test as base, expect } from '@playwright/test';
import * as fs from 'fs';
import * as path from 'path';
import * as crypto from 'crypto';

const coverageDir = path.join(process.cwd(), '.nyc_output');

/**
 * Extended Playwright test fixture that collects Istanbul coverage data
 * from the browser after each test. Only active when the app is built
 * with E2E_COVERAGE=1 (vite-plugin-istanbul instrumentation).
 */
export const test = base.extend({
	page: async ({ page }, use) => {
		await use(page);

		// After each test, pull coverage from the browser if instrumented
		const coverage = await page.evaluate(() => (window as any).__coverage__);
		if (coverage) {
			fs.mkdirSync(coverageDir, { recursive: true });
			const id = crypto.randomUUID();
			fs.writeFileSync(
				path.join(coverageDir, `e2e-${id}.json`),
				JSON.stringify(coverage)
			);
		}
	}
});

export { expect };
