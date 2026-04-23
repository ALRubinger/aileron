import { test, expect } from '@playwright/test';

const API_URL = process.env.AILERON_API_URL || 'http://localhost:8080';

// Use a unique email per test file to avoid collision with other specs.
const EMAIL = 'e2e-auth-spec@aileron.test';
const PASSWORD = 'e2e-auth-password-123';

test.describe('Authentication Flow', () => {
	test('signup creates a new account and login returns a token', async ({ request }) => {
		// Sign up via the API (AILERON_AUTO_VERIFY_EMAIL=true in test stack).
		const signupResp = await request.post(`${API_URL}/auth/signup`, {
			data: { email: EMAIL, password: PASSWORD, display_name: 'Auth Spec' }
		});
		expect([201, 409]).toContain(signupResp.status());

		// Log in.
		const loginResp = await request.post(`${API_URL}/auth/login`, {
			data: { email: EMAIL, password: PASSWORD }
		});
		expect(loginResp.ok()).toBe(true);
		const body = await loginResp.json();
		expect(body.access_token).toBeTruthy();
	});

	test('login page renders and accepts credentials', async ({ page }) => {
		// Sign up via API first.
		const signupResp = await page.request.post(`${API_URL}/auth/signup`, {
			data: { email: EMAIL, password: PASSWORD, display_name: 'Auth Spec' }
		});
		expect([201, 409]).toContain(signupResp.status());

		await page.goto('/login');

		await page.getByLabel('Email').fill(EMAIL);
		await page.getByLabel('Password').fill(PASSWORD);
		await page.getByRole('button', { name: 'Sign in' }).click();

		// After login, should redirect to vault setup or dashboard.
		await expect(page).not.toHaveURL(/\/login/, { timeout: 10000 });
	});
});
