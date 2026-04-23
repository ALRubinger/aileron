import { test as base, expect } from '@playwright/test';

const API_URL = process.env.AILERON_API_URL || 'http://localhost:8080';
const TEST_EMAIL = 'e2e-playwright@aileron.test';
const TEST_PASSWORD = 'e2e-playwright-password-123';
const TEST_DISPLAY_NAME = 'Playwright E2E';

let cachedToken: string | null = null;

/**
 * Signs up a test user and logs in to get a real JWT.
 * Mirrors the Go integration test auth helper (auth_helper_test.go).
 *
 * Requires AILERON_AUTO_VERIFY_EMAIL=true on the server so signup
 * creates active accounts without email confirmation.
 */
async function ensureAuth(): Promise<string> {
	if (cachedToken !== null) return cachedToken;

	// Check if auth is enabled.
	const probe = await fetch(`${API_URL}/v1/intents?workspace_id=default`);
	if (probe.ok) {
		// Auth not enabled — no token needed.
		cachedToken = '';
		return cachedToken;
	}

	// Sign up (201 Created or 409 Conflict if already exists).
	const signupResp = await fetch(`${API_URL}/auth/signup`, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({
			email: TEST_EMAIL,
			password: TEST_PASSWORD,
			display_name: TEST_DISPLAY_NAME
		})
	});

	if (signupResp.status !== 201 && signupResp.status !== 409) {
		const body = await signupResp.text();
		throw new Error(`signup: expected 201 or 409, got ${signupResp.status}: ${body}`);
	}

	// Log in.
	const loginResp = await fetch(`${API_URL}/auth/login`, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({
			email: TEST_EMAIL,
			password: TEST_PASSWORD
		})
	});

	if (!loginResp.ok) {
		const body = await loginResp.text();
		throw new Error(`login: expected 200, got ${loginResp.status}: ${body}`);
	}

	const { access_token } = await loginResp.json();
	cachedToken = access_token;
	return cachedToken;
}

/**
 * Extended Playwright test fixture that authenticates against the real
 * server and injects the JWT into the browser's localStorage before
 * each test.
 */
export const test = base.extend<{ authedPage: import('@playwright/test').Page }>({
	authedPage: async ({ page }, use) => {
		const token = await ensureAuth();

		// Inject auth token into localStorage before navigation
		await page.addInitScript((t: string) => {
			localStorage.setItem('aileron_access_token', t);
		}, token);

		await use(page);
	}
});

export { expect, ensureAuth, API_URL };
